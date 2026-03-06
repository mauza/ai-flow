import { useMemo } from 'react'

function formatElapsed(startedAt) {
  const seconds = Math.floor((Date.now() - new Date(startedAt)) / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  return `${minutes}m ${secs}s`
}

function statusDot(status) {
  switch (status) {
    case 'running':   return 'bg-green-400 animate-pulse'
    case 'completed': return 'bg-green-500'
    case 'failed':    return 'bg-red-500'
    case 'timeout':   return 'bg-yellow-500'
    default:          return 'bg-gray-500'
  }
}

function truncateID(id) {
  return id ? id.slice(0, 8) : '?'
}

export default function SessionList({ sessions, runs, selected, onSelect, onKill }) {
  // Sort sessions newest first
  const sortedSessions = useMemo(
    () => [...sessions].sort((a, b) => new Date(b.started_at) - new Date(a.started_at)),
    [sessions]
  )

  // Runs that aren't currently active sessions
  const activeRunIDs = useMemo(() => new Set(sessions.map(s => s.run_id)), [sessions])
  const recentRuns = useMemo(
    () => runs.filter(r => !activeRunIDs.has(r.id)),
    [runs, activeRunIDs]
  )

  return (
    <div className="w-72 shrink-0 flex flex-col border-r border-gray-800 overflow-hidden">
      {/* Active Sessions */}
      <div className="px-3 py-2 text-xs font-semibold uppercase tracking-wider text-gray-500 border-b border-gray-800">
        Active Sessions
      </div>
      <div className="flex-1 overflow-y-auto">
        {sortedSessions.length === 0 && (
          <div className="px-3 py-4 text-xs text-gray-600">No active sessions</div>
        )}
        {sortedSessions.map(s => {
          const isSelected = selected?.type === 'session' && selected?.id === s.run_id
          return (
            <div
              key={s.run_id}
              onClick={() => onSelect('session', s.run_id)}
              className={`px-3 py-2.5 cursor-pointer border-b border-gray-800/50 hover:bg-gray-800/50 transition-colors ${
                isSelected ? 'bg-gray-800 border-l-2 border-l-green-500' : ''
              }`}
            >
              <div className="flex items-center gap-2 min-w-0">
                <span className={`w-2 h-2 rounded-full shrink-0 bg-green-400 animate-pulse`} />
                <span className="text-sm font-medium text-white truncate">
                  {s.issue_identifier || truncateID(s.issue_id)}
                </span>
                <button
                  onClick={e => { e.stopPropagation(); onKill(s.run_id) }}
                  className="ml-auto shrink-0 px-1.5 py-0.5 text-xs rounded bg-red-900/50 text-red-400 hover:bg-red-800 hover:text-red-200 transition-colors"
                  title="Kill session"
                >
                  ✕
                </button>
              </div>
              <div className="mt-1 flex items-center gap-2 text-xs text-gray-400">
                <span className="truncate">{s.stage_name}</span>
                <span className="ml-auto shrink-0 tabular-nums">{formatElapsed(s.started_at)}</span>
              </div>
              {s.issue_title && (
                <div className="mt-0.5 text-xs text-gray-600 truncate">{s.issue_title}</div>
              )}
            </div>
          )
        })}

        {/* Recent Runs */}
        {recentRuns.length > 0 && (
          <>
            <div className="px-3 py-2 text-xs font-semibold uppercase tracking-wider text-gray-500 border-b border-gray-800 mt-1">
              Recent Runs
            </div>
            {recentRuns.slice(0, 30).map(r => {
              const isSelected = selected?.type === 'run' && selected?.id === r.id
              return (
                <div
                  key={r.id}
                  onClick={() => onSelect('run', r.id)}
                  className={`px-3 py-2.5 cursor-pointer border-b border-gray-800/50 hover:bg-gray-800/50 transition-colors ${
                    isSelected ? 'bg-gray-800 border-l-2 border-l-blue-500' : ''
                  }`}
                >
                  <div className="flex items-center gap-2 min-w-0">
                    <span className={`w-2 h-2 rounded-full shrink-0 ${statusDot(r.status)}`} />
                    <span className="text-sm text-gray-300 truncate font-mono">
                      {truncateID(r.issue_id)}
                    </span>
                    <span className={`ml-auto shrink-0 text-xs px-1.5 py-0.5 rounded ${
                      r.status === 'completed' ? 'bg-green-900/50 text-green-400' :
                      r.status === 'failed'    ? 'bg-red-900/50 text-red-400' :
                      r.status === 'timeout'   ? 'bg-yellow-900/50 text-yellow-400' :
                                                 'bg-gray-800 text-gray-400'
                    }`}>
                      {r.status}
                    </span>
                  </div>
                  <div className="mt-0.5 text-xs text-gray-500 truncate">{r.stage_name}</div>
                </div>
              )
            })}
          </>
        )}
      </div>
    </div>
  )
}
