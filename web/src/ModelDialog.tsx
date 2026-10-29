import { useEffect, useRef } from "react";
import { Check, Copy, X } from "lucide-react";
import { CodeBlock, protocolChoice } from "./CodeBlock";
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
import {
  dialog as dialogClass,
  dialogHeading,
  dialogTitle,
  iconButton,
} from "./ui";

// The models page keeps the protocol choice across openings.
export type { Protocol };

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
      // Preflight drops the margin that centres a modal dialog.
      className={dialogClass()}
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
      <div className={dialogHeading}>
        <h2 id="model-dialog-title" className={dialogTitle}>
          {detail.name}
        </h2>
        <button
          className={iconButton()}
          aria-label="Close model details"
          onClick={onClose}
        >
          <X size={18} />
        </button>
      </div>
      <div className="mt-4.5 flex items-center justify-between gap-2.5 rounded-lg border border-line bg-well py-1 pr-1 pl-2.75">
        <code className="wrap-anywhere">{exposedID(detail)}</code>
        <button
          className={iconButton()}
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
      <div className={protocolChoice}>
        <span>Client protocol</span>
        <Select
          inset
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
    <dl className="mt-5.5 mb-6.5 grid grid-cols-[1fr_1fr] gap-x-5 gap-y-4.5 empty:mt-5 empty:mb-0 max-sm:grid-cols-[1fr]">
      {exposed !== detail.id && (
        <div>
          <dt className="mb-1.5 text-[11px] text-muted">Original ID</dt>
          <dd className="text-[12px] leading-normal wrap-anywhere">
            <code>{detail.id}</code>
          </dd>
        </div>
      )}
      {detail.context > 0 && (
        <div>
          <dt className="mb-1.5 text-[11px] text-muted">Context</dt>
          <dd className="text-[12px] leading-normal wrap-anywhere">
            {detail.context.toLocaleString()} tokens
          </dd>
        </div>
      )}
      {detail.maxOutput !== undefined && detail.maxOutput !== 0 && (
        <div>
          <dt className="mb-1.5 text-[11px] text-muted">Max output</dt>
          <dd className="text-[12px] leading-normal wrap-anywhere">
            {detail.maxOutput.toLocaleString()} tokens
          </dd>
        </div>
      )}
      {detail.inputs !== undefined && detail.inputs.length > 0 && (
        <div>
          <dt className="mb-1.5 text-[11px] text-muted">Input</dt>
          <dd className="text-[12px] leading-normal wrap-anywhere">
            {detail.inputs.join(", ")}
          </dd>
        </div>
      )}
      {detail.reasoningSupported === true && (
        <div>
          <dt className="mb-1.5 text-[11px] text-muted">Reasoning</dt>
          <dd className="text-[12px] leading-normal wrap-anywhere">
            {detail.reasoning !== undefined && detail.reasoning.length > 0
              ? detail.reasoning.join(", ")
              : "Supported"}
          </dd>
        </div>
      )}
    </dl>
  );
}
