import { useEffect, useRef } from 'react'

type Props = { path: string; content: string; line: number }

export default function SourceView({ path, content, line }: Props) {
  const marked = useRef<HTMLSpanElement>(null)

  useEffect(() => {
    marked.current?.scrollIntoView({ block: 'center' })
  }, [path, line])

  return (
    <>
      <h2 className="section-title">
        Source <span className="source-path">{path}</span>
      </h2>
      <pre className="source">
        {content.split('\n').map((text, index) => (
          <span
            key={index}
            ref={index + 1 === line ? marked : null}
            className={index + 1 === line ? 'src-line current' : 'src-line'}
          >
            <span className="src-num">{index + 1}</span>
            {text}
            {'\n'}
          </span>
        ))}
      </pre>
    </>
  )
}
