import { useState, useEffect, useCallback } from 'react'
import SessionList from './components/SessionList.jsx'
import SessionDetail from './components/SessionDetail.jsx'

export default function App() {
  const [sessions, setSessions] = useState([])
  const [runs, setRuns] = useState([])
  const [selected, setSelected] = useState(null)
  // selected = { type: 'session', id: runID } | { type: 'run', id: runID }

  const fetchSessions = useCallback(async () => {
    try {
      const res = await fetch('/dashboard/api/sessions')
      if (res.ok) setSessions(await res.json())
    } catch (_) {}
  }, [])

  const fetchRuns = useCallback(async () => {
    try {
      const res = await fetch('/dashboard/api/runs')
      if (res.ok) setRuns(await res.json())
    } catch (_) {}
  }, [])

  // Initial load + poll every 3 seconds
  useEffect(() => {
    fetchSessions()
    fetchRuns()
    const timer = setInterval(() => {
      fetchSessions()
      fetchRuns()
    }, 3000)
    return () => clearInterval(timer)
  }, [fetchSessions, fetchRuns])

  // If a selected session is no longer active, keep showing but mark done
  const handleSelect = useCallback((type, id) => {
    setSelected({ type, id })
  }, [])

  const handleKill = useCallback(async (runID) => {
    try {
      await fetch(`/dashboard/api/sessions/${runID}`, { method: 'DELETE' })
      fetchSessions()
    } catch (_) {}
  }, [fetchSessions])

  const activeCount = sessions.length

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <header className="flex items-center gap-3 px-4 py-3 bg-gray-900 border-b border-gray-800 shrink-0">
        <span className="text-lg font-semibold tracking-tight text-white">ai-flow</span>
        <span className="text-gray-500 text-sm">dashboard</span>
        {activeCount > 0 && (
          <span className="ml-1 px-2 py-0.5 rounded-full bg-green-700 text-green-100 text-xs font-medium">
            {activeCount} active
          </span>
        )}
      </header>

      {/* Body */}
      <div className="flex flex-1 min-h-0">
        <SessionList
          sessions={sessions}
          runs={runs}
          selected={selected}
          onSelect={handleSelect}
          onKill={handleKill}
        />
        <div className="flex-1 min-w-0">
          {selected ? (
            <SessionDetail
              selected={selected}
              sessions={sessions}
            />
          ) : (
            <div className="flex items-center justify-center h-full text-gray-600 text-sm">
              Select a session or run to view output
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
