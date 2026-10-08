import { lazy, Suspense, useMemo, useState } from "react";
import { parse, stringify } from "yaml";
import { api, type StoryMap } from "../api";
import { Modal, Spinner } from "../ui";

const YamlEditor = lazy(() => import("../flow/YamlEditor"));

/** Edit map.yaml as YAML: personas, the journey, releases and metrics. */
export default function MapEditor({ project, mapID, map, onClose, onSaved }: { project: string; mapID: string; map: StoryMap; onClose: () => void; onSaved: () => void }) {
  const [src, setSrc] = useState(() => stringify(map, { lineWidth: 0 }));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const parsed = useMemo(() => {
    try {
      return { map: parse(src) as StoryMap, error: "" };
    } catch (e) {
      return { map: null, error: (e as Error).message.split("\n")[0] };
    }
  }, [src]);
  const save = async () => {
    if (!parsed.map) return;
    setBusy(true);
    setError("");
    try {
      await api.saveMap(project, mapID, parsed.map);
      onSaved();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal
      title="Edit map"
      onClose={onClose}
      footer={
        <>
          <span className="small faint grow">Personas, phases → activities, releases and metrics. User tasks are separate files.</span>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={busy || !parsed.map} onClick={save}>
            Save
          </button>
        </>
      }
    >
      <div className="entry-yaml tall">
        <Suspense fallback={<Spinner />}>
          <YamlEditor value={src} onChange={setSrc} issues={[]} fileName="map" hint={`product/user-story-maps/${mapID}/map.yaml`} />
        </Suspense>
      </div>
      {(parsed.error || error) && <div className="error-box" style={{ marginTop: 10 }}>{parsed.error || error}</div>}
    </Modal>
  );
}
