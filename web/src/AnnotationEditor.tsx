import type { FormEvent } from 'react'
import {
  ANNOTATION_DESCRIPTION_MAX_LENGTH,
  ANNOTATION_LABEL_MAX_LENGTH,
  limitAnnotationText,
} from './annotations'
import type { Annotation } from './annotations'

type Props = {
  kind: 'node' | 'edge'
  defaultLabel: string
  annotation: Annotation
  loaded: boolean
  dirty: boolean
  saving: boolean
  saved: boolean
  error: string
  onChange: (next: Annotation) => void
  onSave: () => void
}

export default function AnnotationEditor({
  kind,
  defaultLabel,
  annotation,
  loaded,
  dirty,
  saving,
  saved,
  error,
  onChange,
  onSave,
}: Props) {
  const name = kind === 'node' ? 'Node' : 'Edge'
  const labelId = `${kind}-annotation-label`
  const descriptionId = `${kind}-annotation-description`

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    onSave()
  }

  return (
    <form className="topology-annotation" onSubmit={submit}>
      <div className="topology-annotation-fields">
        <label htmlFor={labelId}>
          <span>{name} label</span>
          <input
            id={labelId}
            disabled={!loaded}
            placeholder={defaultLabel}
            value={annotation.label}
            onChange={(event) => onChange({
              ...annotation,
              label: limitAnnotationText(event.target.value, ANNOTATION_LABEL_MAX_LENGTH),
            })}
          />
        </label>

        <label htmlFor={descriptionId}>
          <span>{name} description</span>
          <textarea
            id={descriptionId}
            disabled={!loaded}
            rows={2}
            value={annotation.description}
            onChange={(event) => onChange({
              ...annotation,
              description: limitAnnotationText(
                event.target.value,
                ANNOTATION_DESCRIPTION_MAX_LENGTH,
              ),
            })}
          />
        </label>
      </div>

      <div className="topology-annotation-actions">
        <span className="hint">rainforest.annotations.json</span>
        {error && <span className="bad" role="alert">{error}</span>}
        {saved && <span className="ok" role="status">Saved</span>}
        <button type="submit" disabled={!loaded || !dirty || saving}>
          {saving ? 'Saving…' : 'Save annotations'}
        </button>
      </div>
    </form>
  )
}
