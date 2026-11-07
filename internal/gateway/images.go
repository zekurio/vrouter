package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// imageRequestMax bounds an image request. Edits carry their images inline.
const imageRequestMax = 32 << 20

// The Images API fields vrouter forwards. response_format and stream are
// checked and dropped.
const (
	imageGenerationFields = "model prompt n size quality background output_format output_compression moderation user response_format stream"
	imageEditFields       = imageGenerationFields + " images mask input_fidelity"
)

// imageOperation names the Images API operation of a path, or "" for other
// paths.
func imageOperation(path string) string {
	switch path {
	case "/v1/images/generations":
		return "generations"
	case "/v1/images/edits":
		return "edits"
	}
	return ""
}

// prepareImages selects the image model and the accounts that serve it. The
// Images API needs no translation: ChatGPT serves the same request shape to
// subscription accounts that the OpenAI API serves to API keys.
func (s *server) prepareImages(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt) (*preparedInference, *inferenceError) {
	operation := imageOperation(r.URL.Path)
	payload, ierr := readImageRequest(w, r, operation)
	if ierr != nil {
		return nil, ierr
	}
	model := str(payload["model"])
	if model == "" {
		return nil, &inferenceError{status: 400, message: "Choose a model"}
	}
	attempt.model = model
	state := s.store.snapshot()
	native, ok := inferenceNativeModel(state.Policy, "codex", model)
	if !ok {
		return nil, &inferenceError{status: 404, message: "This image model is disabled. Check disabled models and public aliases."}
	}
	attempt.native = native
	var accounts []storedAccount
	for _, a := range state.Accounts {
		if !a.Disabled && a.Provider == "codex" && routableAuth(a) && accountVisible(a, attempt.principal.UserID) {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return nil, &inferenceError{status: 404, message: "No enabled Codex or OpenAI account can generate images"}
	}
	if err := normalizeImageRequest(payload, operation); err != nil {
		return nil, invalidRequest(err)
	}
	payload["model"] = native
	p := &preparedInference{provider: "codex", native: native, images: operation, routing: state.Routing, accounts: billingPool(accounts, state.Routing)}
	// The Codex CLI never sends an end-user ID, so the subscription backend
	// gets none either.
	if p.accounts[0].AuthMode == "codex" && payload["user"] != nil {
		delete(payload, "user")
		p.ignoredParameters = append(p.ignoredParameters, "user")
	}
	p.payload = make(map[string]json.RawMessage, len(payload))
	for key, value := range payload {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, &inferenceError{status: 400, message: "Invalid image request"}
		}
		p.payload[key] = raw
	}
	return p, nil
}

// readImageRequest reads a JSON request, or the multipart form that OpenAI
// SDKs send for edits.
func readImageRequest(w http.ResponseWriter, r *http.Request, operation string) (map[string]any, *inferenceError) {
	body := &bodyReader{source: r.Body}
	r.Body = http.MaxBytesReader(w, body, imageRequestMax)
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); operation == "edits" && media == "multipart/form-data" {
		payload, err := readImageForm(r)
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			return nil, &inferenceError{status: 400, message: "Image edits are limited to 32 MiB"}
		}
		if body.err != nil {
			return nil, incompleteBody()
		}
		if err != nil {
			return nil, &inferenceError{status: 400, message: err.Error()}
		}
		return payload, nil
	}
	var payload map[string]any
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	if d.Decode(&payload) != nil || payload == nil || d.Decode(&struct{}{}) != io.EOF {
		if body.err != nil {
			return nil, incompleteBody()
		}
		return nil, &inferenceError{status: 400, message: "Expected a JSON image request, at most 32 MiB"}
	}
	return payload, nil
}

// readImageForm turns a multipart edit into the JSON request. Image files
// become data URLs in images, and the mask file becomes mask.
func readImageForm(r *http.Request) (map[string]any, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("Expected a multipart image edit")
	}
	payload := map[string]any{}
	var images []any
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Malformed multipart image edit: %w", err)
		}
		name := part.FormName()
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, fmt.Errorf("Malformed multipart image edit: %w", err)
		}
		var value any
		switch name {
		case "image", "image[]", "mask":
			// The bytes give the type. Clients often label uploads
			// application/octet-stream.
			kind := http.DetectContentType(data)
			if kind != "image/png" && kind != "image/jpeg" && kind != "image/webp" {
				return nil, fmt.Errorf("%s must be a PNG, JPEG or WebP file", strings.TrimSuffix(name, "[]"))
			}
			value = map[string]any{"image_url": "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(data)}
			if name != "mask" {
				images = append(images, value)
				continue
			}
		default:
			value = imageFormValue(name, string(data))
		}
		if _, seen := payload[name]; seen {
			return nil, fmt.Errorf("%s is set more than once", name)
		}
		payload[name] = value
	}
	if images != nil {
		payload["images"] = images
	}
	return payload, nil
}

// imageFormValue gives a form field the JSON type of the same request field.
// A value of the wrong shape stays a string and fails validation.
func imageFormValue(name, value string) any {
	switch name {
	case "n", "output_compression":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return json.Number(strconv.FormatInt(n, 10))
		}
	case "stream":
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	return value
}

// normalizeImageRequest validates the fields vrouter reads and drops the ones
// that ask for something it does not serve. The provider validates the rest.
func normalizeImageRequest(payload map[string]any, operation string) error {
	allowed := imageGenerationFields
	if operation == "edits" {
		allowed = imageEditFields
	}
	known := strings.Fields(allowed)
	for _, key := range slices.Sorted(maps.Keys(payload)) {
		if payload[key] == nil {
			delete(payload, key)
		} else if !slices.Contains(known, key) {
			return fmt.Errorf("vrouter does not support the image request field %s", key)
		}
	}
	if str(payload["prompt"]) == "" {
		return errors.New("prompt must be a non-empty string")
	}
	if err := dropImageDelivery(payload); err != nil {
		return err
	}
	if err := validateImageOptions(payload); err != nil {
		return err
	}
	if operation == "edits" {
		return validateImageEdit(payload)
	}
	return nil
}

func validateImageOptions(payload map[string]any) error {
	if err := validateTokenLimit("n", payload["n"]); err != nil {
		return err
	}
	if value, ok := payload["output_compression"]; ok {
		n, isNumber := protocolNumber(value)
		if !isNumber || !n.IsInt() || n.Sign() < 0 || n.Cmp(big.NewRat(100, 1)) > 0 {
			return errors.New("output_compression must be an integer from 0 to 100")
		}
	}
	for _, key := range []string{"size", "quality", "background", "output_format", "moderation", "input_fidelity", "user"} {
		if _, isText := payload[key].(string); payload[key] != nil && !isText {
			return fmt.Errorf("%s must be a string", key)
		}
	}
	return nil
}

// dropImageDelivery accepts stream and response_format only where they ask
// for the one delivery vrouter serves, a complete b64_json response, and
// removes them.
func dropImageDelivery(payload map[string]any) error {
	switch payload["stream"] {
	case nil, false:
	case true:
		return errors.New("vrouter does not stream image responses. Omit stream.")
	default:
		return errors.New("stream must be a boolean")
	}
	switch payload["response_format"] {
	case nil, "b64_json":
	case "url":
		return errors.New("vrouter returns images as b64_json only. Omit response_format.")
	default:
		return errors.New("response_format must be b64_json")
	}
	delete(payload, "stream")
	delete(payload, "response_format")
	return nil
}

func validateImageEdit(payload map[string]any) error {
	images := list(payload["images"])
	if len(images) == 0 {
		return errors.New("images must list at least one image to edit")
	}
	for _, image := range images {
		if !imageReference(image) {
			return errors.New("Each entry in images needs an image_url or a file_id")
		}
	}
	if mask, ok := payload["mask"]; ok && !imageReference(mask) {
		return errors.New("mask needs an image_url or a file_id")
	}
	return nil
}

// imageReference reports whether a value names an image by URL or file ID.
func imageReference(value any) bool {
	ref := object(value)
	return str(ref["image_url"]) != "" || str(ref["file_id"]) != ""
}

// forwardImages tries each account once, in routing order. Image generation
// has a provider limit of its own, apart from the windows vrouter tracks, so
// those windows neither skip an account nor start a usage reset. An account
// that fails on its own, such as with a 429, passes the request to the next
// one.
func (s *server) forwardImages(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt, prepared *preparedInference) {
	s.tryAccounts(w, r, attempt, routingOrder(prepared.accounts, prepared.routing, s.sequence.Add(1)-1), forwardPlan{
		client: s.slowClient,
		request: func(a storedAccount) (*http.Request, error) {
			return imageRequest(r.Context(), prepared, a, attempt.id)
		},
		invalid: "Invalid image request",
		deliver: func(resp *http.Response, a storedAccount) { deliverImages(w, r, resp, attempt, a) },
		keep:    func(resp *http.Response, a storedAccount) *forwardError { return providerFailure(resp, prepared, a) },
		fail: func(resp *http.Response, a storedAccount) []storedAccount {
			providerFailure(resp, prepared, a).write(w)
			return nil
		},
	})
}

// imageRequest builds the Images API request for one account. Subscription
// accounts use the endpoint the Codex CLI calls for its image tool.
func imageRequest(ctx context.Context, p *preparedInference, a storedAccount, turn string) (*http.Request, error) {
	target := "https://api.openai.com/v1/images/" + p.images
	if a.AuthMode == "codex" {
		target = "https://chatgpt.com/backend-api/codex/images/" + p.images
	}
	req, err := requestJSON(ctx, target, p.payload)
	if err != nil {
		return nil, err
	}
	providerHeaders(req, a)
	if a.AuthMode == "codex" {
		req.Header.Set("X-Codex-Image-Turn-Id", turn)
	}
	return req, nil
}

// deliverImages relays a successful Images API response unchanged and reads
// its usage on the way. The images pass one at a time, so a response with
// several large images is never held whole.
func deliverImages(w http.ResponseWriter, r *http.Request, resp *http.Response, attempt *inferenceAttempt, account storedAccount) {
	defer resp.Body.Close()
	if id := providerRequestID(resp, account); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	client := &relayWriter{w: w}
	body := io.TeeReader(resp.Body, client)
	usage, err := readImageUsage(json.NewDecoder(body))
	if err == nil {
		// Relay whatever follows the object, such as a trailing newline.
		_, err = io.Copy(io.Discard, body)
	}
	switch {
	case client.err != nil || r.Context().Err() != nil:
		attempt.cutShort(reasonClientClosed)
	case err != nil:
		attempt.outcome = outcomeError
		attempt.failure = "Provider image response was malformed or cut off"
	default:
		attempt.bodyComplete = true
		attempt.usage.applyRawUsage(usage)
	}
}

// relayWriter passes a provider body to the client and keeps the first write
// error, so a relay can tell a client that left from a provider that failed.
type relayWriter struct {
	w   io.Writer
	err error
}

func (c *relayWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

// readImageUsage walks an Images API response and returns its usage object.
// Each entry of data is decoded and dropped in turn.
func readImageUsage(d *json.Decoder) (json.RawMessage, error) {
	if err := expectJSONDelim(d, '{'); err != nil {
		return nil, err
	}
	var usage json.RawMessage
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch key {
		case "usage":
			err = d.Decode(&usage)
		case "data":
			err = skipJSONList(d)
		default:
			err = d.Decode(&json.RawMessage{})
		}
		if err != nil {
			return nil, err
		}
	}
	if err := expectJSONDelim(d, '}'); err != nil {
		return nil, err
	}
	return usage, nil
}

func skipJSONList(d *json.Decoder) error {
	if err := expectJSONDelim(d, '['); err != nil {
		return err
	}
	for d.More() {
		if err := d.Decode(&json.RawMessage{}); err != nil {
			return err
		}
	}
	return expectJSONDelim(d, ']')
}

func expectJSONDelim(d *json.Decoder, want json.Delim) error {
	token, err := d.Token()
	if err == nil && token != want {
		err = fmt.Errorf("expected %v", want)
	}
	return err
}
