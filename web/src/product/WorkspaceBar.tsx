import { useState } from "react";
import { ArrowDownToLine, GitCommitHorizontal, Undo2 } from "lucide-react";
import { api, ApiError, type WorkspaceStatus } from "../api";
import { timeAgo } from "../format";
import { Modal, Spinner, useToast } from "../ui";
import DiffView, { diffRows } from "../DiffView";

/**
 * The scratch checkout's state: edits stay on ai-flow's disk until "Commit &
 * push", which makes one commit on the base branch.
 */
export default function WorkspaceBar({ project, status, onChange, defaultMessage }: { project: string; status: WorkspaceStatus; onChange: () => void; defaultMessage: string }) {
  const [reviewing, setReviewing] = useState(false);
  const [pulling, setPulling] = useState(false);
  const toast = useToast();
  const n = status.changes.length;

  const pull = async () => {
    setPulling(true);
    try {
      const r = await api.pull(project);
      toast("ok", r.updated?.length ? `Pulled ${r.updated.length} changed file${r.updated.length === 1 ? "" : "s"}` : "Already up to date");
      onChange();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setPulling(false);
    }
  };

  return (
    <div className="ws-bar" aria-label="Workspace">
      <span className="small muted ws-where">
        <span className="mono">{status.repo}</span> · {status.branch} @ <span className="mono">{status.head.slice(0, 7)}</span>
        {status.pulled_at ? <> · pulled {timeAgo(status.pulled_at)}</> : null}
      </span>
      <span className="spacer" />
      <button className="btn sm" onClick={pull} disabled={pulling} title="Bring in commits pushed since the last pull">
        {pulling ? <Spinner /> : <ArrowDownToLine />}
        Pull
      </button>
      <button className={`btn sm${n ? " primary" : ""}`} disabled={!n} onClick={() => setReviewing(true)}>
        <GitCommitHorizontal />
        {n ? `Commit ${n} change${n === 1 ? "" : "s"}` : "No changes"}
      </button>
      {reviewing && (
        <CommitDialog
          project={project}
          status={status}
          defaultMessage={defaultMessage}
          onClose={() => setReviewing(false)}
          onDone={() => {
            setReviewing(false);
            onChange();
          }}
        />
      )}
    </div>
  );
}

function CommitDialog({ project, status, defaultMessage, onClose, onDone }: { project: string; status: WorkspaceStatus; defaultMessage: string; onClose: () => void; onDone: () => void }) {
  const [message, setMessage] = useState(defaultMessage);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [behind, setBehind] = useState(false);
  const toast = useToast();

  const commit = async () => {
    setBusy(true);
    setError("");
    try {
      const r = await api.commit(project, message);
      toast("ok", `Pushed ${r.sha.slice(0, 7)} to ${status.branch}`);
      onDone();
    } catch (e) {
      setError((e as Error).message);
      setBehind(e instanceof ApiError && e.status === 409);
    } finally {
      setBusy(false);
    }
  };
  const pullThenRetry = async () => {
    setBusy(true);
    try {
      await api.pull(project);
      setBehind(false);
      setError("Pulled the new commits. Review and commit again.");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const discard = async (path?: string) => {
    if (!window.confirm(path ? `Discard your edits to ${path}?` : "Discard every uncommitted edit?")) return;
    try {
      await api.discard(project, path ? [path] : []);
      onDone();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };

  return (
    <Modal
      title={`Commit to ${status.branch}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn ghost danger-text" onClick={() => discard()}>
            <Undo2 />
            Discard all
          </button>
          <span className="spacer" />
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          {behind ? (
            <button className="btn primary" onClick={pullThenRetry} disabled={busy}>
              {busy && <Spinner />}
              Pull
            </button>
          ) : (
            <button className="btn primary" onClick={commit} disabled={busy || !message.trim()}>
              {busy && <Spinner />}
              Commit &amp; push
            </button>
          )}
        </>
      }
    >
      <p className="small muted" style={{ marginTop: 0 }}>
        One commit straight to <b>{status.branch}</b> of {status.repo}. If that branch deploys, make sure its CI ignores <code>product/</code>.
      </p>
      <div className="field">
        <label htmlFor="commit-msg">Message</label>
        <input id="commit-msg" className="input" value={message} onChange={(e) => setMessage(e.target.value)} />
      </div>
      <div className="stack">
        {status.changes.map((c) => (
          <details key={c.path} className="change" open={status.changes.length <= 3}>
            <summary>
              <span className={`chip change-${c.kind}`}>{c.kind}</span>
              <span className="mono grow clamp">{c.path}</span>
              <button className="btn ghost sm" onClick={(e) => (e.preventDefault(), discard(c.path))}>
                Discard
              </button>
            </summary>
            <DiffView rows={diffRows(c.base ?? "", c.work ?? "")} />
          </details>
        ))}
      </div>
      {error && <div className="error-box" style={{ marginTop: 10 }}>{error}</div>}
    </Modal>
  );
}
