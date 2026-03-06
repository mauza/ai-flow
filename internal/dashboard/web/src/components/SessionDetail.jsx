import { useState, useEffect, useRef } from 'react'
import OutputViewer from './OutputViewer.jsx'

function formatTime(iso) {
  if (!iso) return '—'
  return new Date(iso).toLocaleString()
}

function ActiveSessionDetail({ session }) {
  const [events, setEvents] = useState([])
  const [done, setDone] = useState(false)
  const esRef = useRef(null)

  useEffect(() => {
    setEvents([])
    setDone(false)

    const es = new EventSource(`/dashboard/api/sessions/${session.run_id}/stream`)

    es.addEventListener('init', e => {
      try { setEvents(JSON.parse(e.data)) } catch (_) {}
    })

    es.addEventListener('output', e => {
      try { setEvents(prev => [...prev, JSON.parse(e.data)]) } catch (_) {}
    })

    es.addEventListener('done', () => {
      setDone(true)
      es.close()
    })

    es.onerror = () => {
      setDone(true)
      es.close()
    }

    esRef.current = es
    return () => es.close()
  }, [session.run_id])

  return (
    <div className="flex flex-col h-full">
      {/* Info bar */}
      <div className="px-4 py-3 bg-gray-900 border-b border-gray-800 shrink-0">
        <div className="flex items-center gap-2 flex-wrap">
          {session.issue_identifier && (
            <span className="font-semibold text-white">{session.issue_identifier}</span>
          )}
          <span className="text-gray-400 text-sm truncate flex-1">{session.issue_title}</span>
          <span className={`text-xs px-2 py-0.5 rounded-full ${
            done ? 'bg-gray-700 text-gray-300' : 'bg-green-800 text-green-200 animate-pulse'
          }`}>
            {done ? 'done' : 'running'}
          </span>
        </div>
        <div className="mt-1 flex gap-4 text-xs text-gray-500">
          <span>Stage: <span className="text-gray-300">{session.stage_name}</span></span>
          <span>Started: <span className="text-gray-300">{formatTime(session.started_at)}</span></span>
          {session.issue_url && (
            <a href={session.issue_url} target="_blank" rel="noreferrer"
               className="text-blue-400 hover:underline">Linear ↗</a>
          )}
        </div>
      </div>

      {/* Output */}
      <div className="flex-1 min-h-0">
        <OutputViewer events={events} autoScroll={!done} />
      </div>
    </div>
  )
}

function CompletedRunDetail({ runID }) {
  const [run, setRun] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    setLoading(true)
    fetch(`/dashboard/api/runs/${runID}`)
      .then(r => r.ok ? r.json() : null)
      .then(data => { setRun(data); setLoading(false) })
      .catch(() => setLoading(false))
  }, [runID])

  if (loading) {
    return <div className="flex items-center justify-center h-full text-gray-600 text-sm">Loading…</div>
  }
  if (!run) {
    return <div className="flex items-center justify-center h-full text-gray-600 text-sm">Run not found</div>
  }

  // Convert stored output string to an array of events for the viewer
  const events = run.output
    ? [{ type: 'stdout', data: run.output, time: run.ended_at }]
    : []
  const errEvents = run.error
    ? [{ type: 'stderr', data: run.error, time: run.ended_at }]
    : []

  return (
    <div className="flex flex-col h-full">
      {/* Info bar */}
      <div className="px-4 py-3 bg-gray-900 border-b border-gray-800 shrink-0">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="font-mono text-gray-400 text-sm">{run.issue_id?.slice(0, 8)}</span>
          <span className={`text-xs px-2 py-0.5 rounded-full ${
            run.status === 'completed' ? 'bg-green-900 text-green-300' :
            run.status === 'failed'    ? 'bg-red-900 text-red-300' :
            run.status === 'timeout'   ? 'bg-yellow-900 text-yellow-300' :
                                         'bg-gray-700 text-gray-300'
          }`}>
            {run.status}{run.exit_code != null ? ` (exit ${run.exit_code})` : ''}
          </span>
        </div>
        <div className="mt-1 flex gap-4 text-xs text-gray-500 flex-wrap">
          <span>Stage: <span className="text-gray-300">{run.stage_name}</span></span>
          <span>Started: <span className="text-gray-300">{formatTime(run.started_at)}</span></span>
          {run.ended_at && (
            <span>Ended: <span className="text-gray-300">{formatTime(run.ended_at)}</span></span>
          )}
          {run.pr_url && (
            <a href={run.pr_url} target="_blank" rel="noreferrer"
               className="text-blue-400 hover:underline">PR ↗</a>
          )}
        </div>
      </div>

      {/* Output */}
      <div className="flex-1 min-h-0">
        <OutputViewer events={[...events, ...errEvents]} autoScroll={false} />
      </div>
    </div>
  )
}

export default function SessionDetail({ selected, sessions }) {
  if (selected.type === 'session') {
    const session = sessions.find(s => s.run_id === selected.id)
    if (!session) {
      // Session may have just ended; show completed run view
      return <CompletedRunDetail runID={selected.id} />
    }
    return <ActiveSessionDetail session={session} />
  }

  if (selected.type === 'run') {
    return <CompletedRunDetail runID={selected.id} />
  }

  return null
}
