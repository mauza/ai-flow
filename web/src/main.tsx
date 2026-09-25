import { lazy, StrictMode, Suspense, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { createBrowserRouter, RouterProvider } from "react-router-dom";
import "./styles.css";
import Shell from "./Shell";
import Board from "./pages/Board";
import Flows from "./pages/Flows";
import Runs from "./pages/Runs";
import Catalog from "./pages/Catalog";
import { Spinner, ToastProvider } from "./ui";

// The graph pages pull in ELK and CodeMirror; load them on demand.
const FlowPage = lazy(() => import("./pages/FlowPage"));
const RunPage = lazy(() => import("./pages/RunPage"));

const Loading = ({ children }: { children: ReactNode }) => (
  <Suspense
    fallback={
      <div className="center-fill">
        <Spinner lg />
      </div>
    }
  >
    {children}
  </Suspense>
);

const router = createBrowserRouter([
  {
    path: "/",
    element: <Shell />,
    children: [
      { index: true, element: <Board /> },
      { path: "flows", element: <Flows /> },
      { path: "flows/:name", element: <Loading><FlowPage /></Loading> },
      { path: "runs", element: <Runs /> },
      { path: "runs/:id", element: <Loading><RunPage /></Loading> },
      { path: "catalog", element: <Catalog /> },
      { path: "*", element: <div className="page">Not found.</div> },
    ],
  },
]);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ToastProvider>
      <RouterProvider router={router} />
    </ToastProvider>
  </StrictMode>,
);
