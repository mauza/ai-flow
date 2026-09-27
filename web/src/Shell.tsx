import { NavLink, Outlet } from "react-router-dom";
import { Boxes, KanbanSquare, Library, Workflow, Play } from "lucide-react";
import { api } from "./api";
import { useLive, useResource } from "./hooks";

export default function Shell() {
  const live = useLive();
  const overview = useResource(() => api.overview(), []);
  const runs = useResource(() => api.runs(), [], (e) => e.type === "run");
  const active = runs.data?.filter((r) => r.status === "running" || r.status === "waiting" || r.status === "queued").length ?? 0;
  const waiting = runs.data?.filter((r) => r.status === "waiting").length ?? 0;

  return (
    <div className="shell">
      <nav className="sidebar" aria-label="Main">
        <div className="brand">
          <img src="/favicon.svg" alt="" />
          <div>
            ai-flow
            <small>flows as state machines</small>
          </div>
        </div>
        <NavLink to="/" end className="nav-link">
          <KanbanSquare />
          <span className="lbl">Board</span>
        </NavLink>
        <NavLink to="/flows" className="nav-link">
          <Workflow />
          <span className="lbl">Flows</span>
        </NavLink>
        <NavLink to="/runs" className="nav-link">
          <Play />
          <span className="lbl">Runs</span>
          {active > 0 && (
            <span className="count" title={waiting ? `${waiting} waiting for you` : undefined}>
              {waiting > 0 ? `${active} · ${waiting}✋` : active}
            </span>
          )}
        </NavLink>
        <NavLink to="/catalog" className="nav-link">
          <Library />
          <span className="lbl">Catalog</span>
        </NavLink>
        <div className="sidebar-foot">
          <span className="live" title={live ? "Receiving live updates" : "Reconnecting…"}>
            <span className={`live-dot${live ? " on" : ""}`} />
            {live ? "Live" : "Reconnecting…"}
          </span>
          {overview.data && (
            <span className="live">
              <Boxes size={13} />
              {overview.data.local ? "local processes" : "kubernetes jobs"}
            </span>
          )}
        </div>
      </nav>
      <main className="main">
        {overview.error === "unauthorized" ? (
          <div className="center-fill">
            <div className="empty">
              <Boxes />
              <h3>This ai-flow needs a token</h3>
              <p>
                Open it once with <code>?token=&lt;your token&gt;</code> added to the URL. The token is the value of the env var named by{" "}
                <code>server.authTokenEnv</code>.
              </p>
            </div>
          </div>
        ) : (
          <Outlet />
        )}
      </main>
    </div>
  );
}
