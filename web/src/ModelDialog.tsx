import { useEffect, useRef } from "react";
import { Check, Copy, X } from "lucide-react";
import { CodeBlock } from "./CodeBlock";
import { ConnectionTest } from "./ConnectionTest";
import { isDialogBackdropClick } from "./dialog";
import type { DraftView, ManagedModel } from "./models";
import {
  curlExample,
  protocolPath,
  protocols,
  type Protocol,
} from "./protocols";
import { Select } from "./Select";

type DetailProps = {
  view: DraftView;
  endpoint: string;
  protocol: Protocol;
  onProtocol: (protocol: Protocol) => void;
  // The ID copied last, shown with a check mark for a moment.
  copied: string;
  onCopyID: (id: string) => void;
  copy: (value: string) => Promise<boolean>;
  onClose: () => void;
};

// Details, a curl example, and a connection test for the selected model.
export function ModelDialog({
  selected,
  detail,
  ...props
}: DetailProps & {
  // Key of the selected model. The dialog is open while it is set.
  selected: string | null;
  detail: ManagedModel | null;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (selected === null || selected === "") dialog.current?.close();
    else dialog.current?.showModal();
  }, [selected]);
  return (
    // oxlint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions -- backdrop click; Escape is handled by onCancel
    <dialog
      ref={dialog}
      onCancel={props.onClose}
      onClick={(e) => {
        if (isDialogBackdropClick(e)) props.onClose();
      }}
      aria-labelledby="model-dialog-title"
    >
      {detail && <ModelDetail detail={detail} {...props} />}
    </dialog>
  );
}

function ModelDetail({
  detail,
  view,
  endpoint,
  protocol,
  onProtocol,
  copied,
  onCopyID,
  copy,
  onClose,
}: DetailProps & { detail: ManagedModel }) {
  const { exposedID } = view;
  // The test runs in the browser, which can only call its own origin. When the
  // public address is another origin, the same server answers on this one.
  const testBase = endpoint.startsWith(`${location.origin}/`)
    ? endpoint
    : `${location.origin}/v1`;
  // Drafts are not live until saved, so a test can only use the saved state.
  const testBlock = (m: ManagedModel) =>
    view.isChanged(m)
      ? "Save your changes to this model first."
      : m.enabled
        ? undefined
        : "Enable this model and save to test it.";
  return (
    <>
      <div className="dialog-heading">
        <h2 id="model-dialog-title">{detail.name}</h2>
        <button
          className="icon-button"
          aria-label="Close model details"
          onClick={onClose}
        >
          <X size={18} />
        </button>
      </div>
      <div className="model-id">
        <code>{exposedID(detail)}</code>
        <button
          className="icon-button"
          aria-label="Copy model ID"
          onClick={() => onCopyID(exposedID(detail))}
        >
          {copied === exposedID(detail) ? (
            <Check size={15} />
          ) : (
            <Copy size={15} />
          )}
        </button>
      </div>
      <ModelFacts detail={detail} exposed={exposedID(detail)} />
      <div className="protocol-choice">
        <span>Client protocol</span>
        <Select
          label="Client protocol"
          value={protocol}
          options={protocols}
          onChange={onProtocol}
        />
      </div>
      <CodeBlock
        code={curlExample(endpoint, protocol, exposedID(detail))}
        method="POST"
        path={`/v1${protocolPath(protocol)}`}
        copy={copy}
      />
      <ConnectionTest
        base={testBase}
        protocol={protocol}
        model={exposedID(detail)}
        provider={detail.provider}
        blocked={testBlock(detail)}
      />
    </>
  );
}

function ModelFacts({
  detail,
  exposed,
}: {
  detail: ManagedModel;
  exposed: string;
}) {
  return (
    <dl className="model-facts">
      {exposed !== detail.id && (
        <div>
          <dt>Original ID</dt>
          <dd>
            <code>{detail.id}</code>
          </dd>
        </div>
      )}
      {detail.context > 0 && (
        <div>
          <dt>Context</dt>
          <dd>{detail.context.toLocaleString()} tokens</dd>
        </div>
      )}
      {detail.maxOutput !== undefined && detail.maxOutput !== 0 && (
        <div>
          <dt>Max output</dt>
          <dd>{detail.maxOutput.toLocaleString()} tokens</dd>
        </div>
      )}
      {detail.inputs !== undefined && detail.inputs.length > 0 && (
        <div>
          <dt>Input</dt>
          <dd>{detail.inputs.join(", ")}</dd>
        </div>
      )}
      {detail.reasoningSupported === true && (
        <div>
          <dt>Reasoning</dt>
          <dd>
            {detail.reasoning !== undefined && detail.reasoning.length > 0
              ? detail.reasoning.join(", ")
              : "Supported"}
          </dd>
        </div>
      )}
    </dl>
  );
}
