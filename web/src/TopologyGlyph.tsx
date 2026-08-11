import type { GlyphKind } from './topology'

type Props = {
  kind: GlyphKind
  x: number
  y: number
  size: number
}

export default function TopologyGlyph({ kind, x, y, size }: Props) {
  let glyph
  switch (kind) {
    case 'network':
      glyph = <><circle cx="8" cy="8" r="2" /><circle cx="16" cy="8" r="2" /><circle cx="12" cy="16" r="2" /><path d="M9.7 9.1 11 14M14.3 9.1 13 14M10 8h4" /></>
      break
    case 'subnet':
      glyph = <><rect x="6" y="6" width="12" height="12" rx="2" /><path d="M6 11h12M11 11v7" /></>
      break
    case 'route':
      glyph = <><path d="M6 17h3a3 3 0 0 0 3-3V8M9 11l3-3 3 3M12 14a3 3 0 0 0 3 3h3" /></>
      break
    case 'gateway':
      glyph = <><path d="M6 18V7l6-2 6 2v11M9 18v-7h6v7M4 18h16" /></>
      break
    case 'security':
      glyph = <path d="M12 4 18 7v4c0 4-2.4 7-6 9-3.6-2-6-5-6-9V7l6-3Z" />
      break
    case 'compute':
      glyph = <><rect x="7" y="7" width="10" height="10" rx="2" /><path d="M9 4v3M15 4v3M9 17v3M15 17v3M4 9h3M4 15h3M17 9h3M17 15h3" /></>
      break
    case 'interface':
      glyph = <><path d="M5 12h5M14 12h5M10 8v8M14 8v8" /><circle cx="12" cy="12" r="2" /></>
      break
    case 'database':
      glyph = <><ellipse cx="12" cy="7" rx="6" ry="3" /><path d="M6 7v10c0 1.7 2.7 3 6 3s6-1.3 6-3V7M6 12c0 1.7 2.7 3 6 3s6-1.3 6-3" /></>
      break
    case 'storage':
      glyph = <><path d="M5 8h14v11H5zM5 8l3-3h8l3 3M9 12h6" /></>
      break
    default:
      glyph = <><rect x="6" y="6" width="5" height="5" rx="1" /><rect x="13" y="6" width="5" height="5" rx="1" /><rect x="6" y="13" width="5" height="5" rx="1" /><rect x="13" y="13" width="5" height="5" rx="1" /></>
  }

  return (
    <svg
      className={`topology-tile tile-${kind}`}
      x={x}
      y={y}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      aria-hidden="true"
    >
      <rect className="topology-tile-surface" width="24" height="24" rx="5" />
      <g className="topology-glyph">{glyph}</g>
    </svg>
  )
}
