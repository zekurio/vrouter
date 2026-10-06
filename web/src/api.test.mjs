import assert from "node:assert/strict";
import { test } from "node:test";
import { createClient, gatewayHeader, isStale, pickGateway } from "./api.ts";

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), { status });
// A fetch stand-in that records calls and settles them on demand.
function recorder() {
  const calls = [];
  const fetch = (path, init) =>
    new Promise((resolve, reject) => {
      const call = { path, init, resolve, reject };
      init.signal.addEventListener("abort", () => reject(init.signal.reason));
      calls.push(call);
    });
  return { calls, fetch };
}

test("every request carries the gateway header and the admin token", async () => {
  const { calls, fetch } = recorder();
  let token = "secret";
  const client = createClient({ gateway: "gw_a", token: () => token, fetch });
  const state = client.request("/api/state");
  const patch = client.request("/api/accounts", "PATCH", { id: "1" });
  for (const call of calls) {
    assert.equal(call.init.headers[gatewayHeader], "gw_a");
    assert.equal(call.init.headers.Authorization, "Bearer secret");
  }
  assert.equal(calls[1].init.headers["Content-Type"], "application/json");
  assert.equal(calls[1].init.body, '{"id":"1"}');
  calls[0].resolve(json({ mode: "live" }));
  calls[1].resolve(json({ ok: true }));
  assert.deepEqual(await state, { mode: "live" });
  await patch;

  token = "";
  const cookie = client.request("/api/keys");
  assert.equal("Authorization" in calls[2].init.headers, false);
  calls[2].resolve(json({ keys: [] }));
  await cookie;
});

test("a client without a gateway sends no gateway header", async () => {
  const { calls, fetch } = recorder();
  const client = createClient({ token: () => "", fetch });
  const pending = client.request("/api/gateways");
  assert.equal(gatewayHeader in calls[0].init.headers, false);
  calls[0].resolve(json({ gateways: [] }));
  await pending;
});

test("closing aborts reads in flight and refuses new ones", async () => {
  const { calls, fetch } = recorder();
  const client = createClient({ gateway: "gw_a", token: () => "", fetch });
  const pending = client.request("/api/state");
  client.close();
  assert.equal(calls[0].init.signal.aborted, true);
  await assert.rejects(pending, isStale);
  await assert.rejects(client.request("/api/telemetry"), isStale);
  await assert.rejects(client.request("/api/keys", "POST", {}), isStale);
  assert.equal(calls.length, 1);
});

test("a read that answers after close is dropped, not delivered", async () => {
  const calls = [];
  const fetch = (path, init) =>
    new Promise((resolve) => calls.push({ path, init, resolve }));
  const client = createClient({ gateway: "gw_a", token: () => "", fetch });
  const pending = client.request("/api/state");
  client.close();
  calls[0].resolve(json({ mode: "live" }));
  await assert.rejects(pending, isStale);
});

test("a write sent before close finishes, and DELETE still goes out after", async () => {
  const { calls, fetch } = recorder();
  const client = createClient({ gateway: "gw_a", token: () => "", fetch });
  const started = client.request("/api/oauth/codex", "POST", {});
  client.close();
  assert.equal(calls[0].init.signal.aborted, false);
  calls[0].resolve(json({ id: "s1" }));
  assert.deepEqual(await started, { id: "s1" });

  const released = client.request("/api/oauth/sessions/s1", "DELETE");
  assert.equal(calls[1].init.method, "DELETE");
  assert.equal(calls[1].init.headers[gatewayHeader], "gw_a");
  calls[1].resolve(json({ ok: true }));
  await released;
});

test("errors keep the server message and status, and 401 is reported once open", async () => {
  let unauthorized = 0;
  const responses = [
    json({ error: "gateway not found" }, 404),
    json(null, 401),
    new Response("oops", { status: 500 }),
  ];
  const client = createClient({
    gateway: "gw_a",
    token: () => "",
    onUnauthorized: () => unauthorized++,
    fetch: async () => responses.shift(),
  });
  await assert.rejects(client.request("/api/state"), {
    message: "gateway not found",
    status: 404,
  });
  await assert.rejects(client.request("/api/state"), { status: 401 });
  assert.equal(unauthorized, 1);
  await assert.rejects(client.request("/api/state"), {
    message: "Request failed (500).",
    status: 500,
  });
});

test("network failures get a readable message", async () => {
  const client = createClient({
    token: () => "",
    fetch: async () => {
      throw new TypeError("fetch failed");
    },
  });
  await assert.rejects(client.request("/api/auth"), {
    message: "Could not reach vrouter.",
  });
});

const gw = (id, ownerId, createdAt) => ({ id, name: id, ownerId, createdAt });

test("pickGateway keeps a stored choice only while it exists", () => {
  const list = [gw("a", "u1", "2026-01-02"), gw("b", "u1", "2026-01-03")];
  assert.equal(pickGateway(list, "b", "oidc"), "b");
  assert.equal(pickGateway(list, "gone", "oidc"), null);
});

test("pickGateway never opens one of several gateways unasked under OIDC", () => {
  const list = [
    gw("legacy", "local-admin", "2026-01-01"),
    gw("mine", "u1", "2026-01-02"),
  ];
  assert.equal(pickGateway(list, null, "oidc"), null);
  assert.equal(pickGateway([list[1]], null, "oidc"), "mine");
  assert.equal(pickGateway([], null, "oidc"), null);
});

test("pickGateway opens the legacy gateway for local and token access", () => {
  const list = [
    gw("newer", "local-admin", "2026-03-01"),
    gw("other", "u1", "2025-01-01"),
    gw("legacy", "local-admin", "2026-01-01"),
  ];
  assert.equal(pickGateway(list, null, "local"), "legacy");
  assert.equal(pickGateway(list, null, "token"), "legacy");
  assert.equal(pickGateway([list[1]], null, "token"), "other");
  assert.equal(pickGateway([], null, "local"), null);
});
