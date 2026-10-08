import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Send } from "lucide-react";
import { api, type StoryMap, type UserTask } from "../api";
import { Modal, Spinner, useToast } from "../ui";
import { scopeTasks } from "./model";

/** Send a user task, an activity or a phase to a flow: ai-flow plans it as a task. */
export default function SendDialog({
  project,
  mapID,
  map,
  tasks,
  kind,
  id,
  title,
  onClose,
}: {
  project: string;
  mapID: string;
  map: StoryMap;
  tasks: UserTask[];
  kind: "task" | "activity" | "phase";
  id: string;
  title: string;
  onClose: () => void;
}) {
  const [release, setRelease] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const toast = useToast();
  const navigate = useNavigate();
  const covered = scopeTasks(map, tasks, kind, id, release);

  const send = async () => {
    setBusy(true);
    setError("");
    try {
      const t = await api.sendToFlow(project, mapID, kind, id, release, true);
      toast("ok", "Sent: the planner is drafting a flow");
      onClose();
      navigate(`/?task=${t.id}`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={`Send ${kind === "task" ? "user task" : kind} to a flow`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={busy || covered.length === 0} onClick={send}>
            {busy ? <Spinner /> : <Send />}
            Plan a flow
          </button>
        </>
      }
    >
      <p style={{ marginTop: 0 }}>
        <b>{title}</b>
      </p>
      {kind !== "task" && (
        <div className="field">
          <label htmlFor="send-release">Release</label>
          <select id="send-release" className="input" value={release} onChange={(e) => setRelease(e.target.value)}>
            <option value="">All releases</option>
            {map.releases.map((r) => (
              <option key={r.id} value={r.id}>
                {r.title}
              </option>
            ))}
          </select>
        </div>
      )}
      <div className="label">{covered.length ? `Covers ${covered.length} user task${covered.length === 1 ? "" : "s"}` : "No open user tasks in this scope"}</div>
      <ul className="small scope-list">
        {covered.map((t) => (
          <li key={t.id}>{t.title}</li>
        ))}
      </ul>
      <p className="small muted">
        The planner gets each task's story, acceptance criteria and place in the journey, plus a pointer to <code>product/</code>. The flow starts by itself
        if the project's start mode is auto; otherwise start it from the board. The committed version of the map is what flows see in the repository.
      </p>
      {error && <div className="error-box">{error}</div>}
    </Modal>
  );
}
