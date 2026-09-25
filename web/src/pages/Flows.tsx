import { useNavigate } from "react-router-dom";
import { Workflow } from "lucide-react";
import { api } from "../api";
import { useResource } from "../hooks";
import { timeAgo } from "../format";
import { Empty, Spinner } from "../ui";

export default function Flows() {
  const flows = useResource(() => api.flows(), [], (e) => e.type === "flow");
  const navigate = useNavigate();
  return (
    <div className="page">
      <div className="page-head">
        <h1>Flows</h1>
        <span className="sub">One state machine per task. Every save is a new version; runs pin the version they started with.</span>
      </div>
      {flows.error && <div className="error-box mb">{flows.error}</div>}
      {!flows.data ? (
        <Spinner lg />
      ) : flows.data.length === 0 ? (
        <div className="card">
          <Empty icon={Workflow} title="No flows yet">
            <p>Create a task on the board and the planner will draft a flow for it.</p>
          </Empty>
        </div>
      ) : (
        <div className="card" style={{ overflowX: "auto" }}>
          <table className="table">
            <thead>
              <tr>
                <th>Flow</th>
                <th>Version</th>
                <th className="hide-sm">Project</th>
                <th className="hide-sm">Last change</th>
                <th>Updated</th>
              </tr>
            </thead>
            <tbody>
              {flows.data.map((f) => (
                <tr key={f.name} onClick={() => navigate(`/flows/${f.name}`)}>
                  <td className="mono">{f.name}</td>
                  <td className="num">v{f.version}</td>
                  <td className="hide-sm">{f.project}</td>
                  <td className="muted small hide-sm">{f.note || f.created_by}</td>
                  <td className="muted small nowrap">{timeAgo(f.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
