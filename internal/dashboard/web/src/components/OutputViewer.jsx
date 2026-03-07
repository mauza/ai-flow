import { useEffect, useRef } from 'react'

import { useState } from 'react'

function StdinBlock({ data }) {
  const [expanded, setExpanded] = useState(false)
  const lines = data.split('\n')
  const preview = lines.slice(0, 3).join('\n')
  const isLong = lines.length > 3

  return (
    <div className="mb-3 rounded border border-cyan-900/50 bg-cyan-950/30">
      <button
        onClick={() => setExpanded(e => !e)}
        className="w-full flex items-center gap-2 px-3 py-1.5 text-xs text-cyan-400 hover:text-cyan-200 text-left"
      >
        <span className="font-semibold tracking-wide">▶ PROMPT</span>
        <span className="text-cyan-700">{lines.length} lines</span>
        <span className="ml-auto">{expanded ? '▲ collapse' : '▼ expand'}</span>
      </button>
      {expanded ? (
        <pre className="px-3 pb-3 text-cyan-300/80 whitespace-pre-wrap break-words leading-relaxed text-xs">
          {data}
        </pre>
      ) : (
        <pre
          className="px-3 pb-2 text-cyan-300/50 whitespace-pre-wrap break-words leading-relaxed text-xs cursor-pointer"
          onClick={() => setExpanded(true)}
        >
          {preview}{isLong ? '\n…' : ''}
        </pre>
      )}
    </div>
  )
}

function lineClass(type) {
  switch (type) {
    case 'stderr': return 'text-red-400'
    case 'system': return 'text-gray-500 italic'
    default:       return 'text-gray-100'
  }
}

export default function OutputViewer({ events, autoScroll = true }) {
  const bottomRef = useRef(null)

  useEffect(() => {
    if (autoScroll) {
      bottomRef.current?.scrollIntoView({ behavior: 'instant' })
    }
  }, [events, autoScroll])

  if (!events || events.length === 0) {
    return (
      <div className="font-mono text-sm bg-black h-full p-4 overflow-auto text-gray-600">
        Waiting for output...
      </div>
    )
  }

  return (
    <div className="font-mono text-sm bg-black h-full p-4 overflow-auto">
      {events.map((evt, i) => {
        if (evt.type === 'stdin') {
          return <StdinBlock key={i} data={evt.data} />
        }
        return (
          <pre
            key={i}
            className={`whitespace-pre-wrap break-words leading-relaxed ${lineClass(evt.type)}`}
          >
            {evt.data}
          </pre>
        )
      })}
      <div ref={bottomRef} />
    </div>
  )
}
