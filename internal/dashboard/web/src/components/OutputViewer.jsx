import { useEffect, useRef } from 'react'

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
      {events.map((evt, i) => (
        <pre
          key={i}
          className={`whitespace-pre-wrap break-words leading-relaxed ${lineClass(evt.type)}`}
        >
          {evt.data}
        </pre>
      ))}
      <div ref={bottomRef} />
    </div>
  )
}
