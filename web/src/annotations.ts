export const ANNOTATION_KEY_MAX_LENGTH = 1024
export const ANNOTATION_LABEL_MAX_LENGTH = 80
export const ANNOTATION_DESCRIPTION_MAX_LENGTH = 4000

export type Annotation = {
  label: string
  description: string
}

export type AnnotationsDocument = {
  version: 1
  nodes: Record<string, Annotation>
  edges: Record<string, Annotation>
}

export type AnnotationTarget = {
  kind: 'node' | 'edge'
  key: string
}

const EMPTY_ANNOTATION: Annotation = { label: '', description: '' }

export function limitAnnotationText(value: string, maxLength: number) {
  return [...value].slice(0, maxLength).join('')
}

export function emptyAnnotationsDocument(): AnnotationsDocument {
  return { version: 1, nodes: {}, edges: {} }
}

function entriesFor(document: AnnotationsDocument, kind: AnnotationTarget['kind']) {
  return kind === 'node' ? document.nodes : document.edges
}

export function annotationFor(document: AnnotationsDocument, target: AnnotationTarget): Annotation {
  return entriesFor(document, target.kind)[target.key] ?? EMPTY_ANNOTATION
}

export function annotationLabel(
  document: AnnotationsDocument,
  target: AnnotationTarget,
  fallback: string,
) {
  const label = annotationFor(document, target).label
  return label.trim() === '' ? fallback : label
}

export function updateAnnotation(
  document: AnnotationsDocument,
  target: AnnotationTarget,
  annotation: Annotation,
): AnnotationsDocument {
  const entries = { ...entriesFor(document, target.kind) }
  if (annotation.label === '' && annotation.description === '') {
    delete entries[target.key]
  } else {
    entries[target.key] = annotation
  }
  return target.kind === 'node'
    ? { ...document, nodes: entries }
    : { ...document, edges: entries }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function normalizeEntries(value: unknown): Record<string, Annotation> | null {
  if (!isRecord(value)) return null
  const entries: Array<[string, Annotation]> = []
  for (const [key, entry] of Object.entries(value)) {
    const keyLength = [...key].length
    if (keyLength === 0 || keyLength > ANNOTATION_KEY_MAX_LENGTH) return null
    if (!isRecord(entry) || Object.keys(entry).some((field) => field !== 'label' && field !== 'description')) {
      return null
    }
    const { label = '', description = '' } = entry
    if (typeof label !== 'string' || typeof description !== 'string') return null
    if (
      [...label].length > ANNOTATION_LABEL_MAX_LENGTH ||
      [...description].length > ANNOTATION_DESCRIPTION_MAX_LENGTH
    ) return null
    if (label === '' && description === '') continue
    entries.push([key, { label, description }])
  }
  return Object.fromEntries(entries)
}

function normalizeDocument(body: unknown): AnnotationsDocument | null {
  if (
    !isRecord(body) ||
    body.version !== 1 ||
    Object.keys(body).some((field) => field !== 'version' && field !== 'nodes' && field !== 'edges')
  ) return null
  const nodes = normalizeEntries(body.nodes)
  const edges = normalizeEntries(body.edges)
  return nodes && edges ? { version: 1, nodes, edges } : null
}

async function responseDocument(response: Response): Promise<AnnotationsDocument> {
  let body: unknown
  try {
    body = await response.json()
  } catch {
    if (!response.ok) throw new Error(`request failed (${response.status})`)
    throw new Error('invalid annotations response')
  }
  if (!response.ok) {
    const error = isRecord(body) ? body.error : undefined
    throw new Error(typeof error === 'string' && error !== '' ? error : `request failed (${response.status})`)
  }
  const document = normalizeDocument(body)
  if (!document) throw new Error('invalid annotations response')
  return document
}

export async function getAnnotations(fetcher: typeof fetch = fetch) {
  return responseDocument(await fetcher('/api/annotations', { credentials: 'same-origin' }))
}

export async function putAnnotations(document: AnnotationsDocument, fetcher: typeof fetch = fetch) {
  return responseDocument(await fetcher('/api/annotations', {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(document),
  }))
}

export type AnnotationsState = {
  document: AnnotationsDocument
  revision: number
  dirty: boolean
  loaded: boolean
  saved: boolean
  loadingRequest: { id: number; revision: number } | null
  savingRequest: { id: number; revision: number } | null
  error: string
}

export type AnnotationsAction =
  | { type: 'load-start'; requestId: number; revision: number }
  | { type: 'load-success'; requestId: number; revision: number; document: AnnotationsDocument }
  | { type: 'load-failure'; requestId: number; revision: number; error: string }
  | { type: 'edit'; target: AnnotationTarget; annotation: Annotation }
  | { type: 'save-start'; requestId: number; revision: number }
  | { type: 'save-success'; requestId: number; revision: number; document: AnnotationsDocument }
  | { type: 'save-failure'; requestId: number; revision: number; error: string }

export function createAnnotationsState(
  document: AnnotationsDocument = emptyAnnotationsDocument(),
): AnnotationsState {
  return {
    document,
    revision: 0,
    dirty: false,
    loaded: false,
    saved: false,
    loadingRequest: null,
    savingRequest: null,
    error: '',
  }
}

function ownsRequest(
  request: { id: number; revision: number } | null,
  action: { requestId: number; revision: number },
) {
  return request?.id === action.requestId && request.revision === action.revision
}

export function annotationsReducer(
  state: AnnotationsState,
  action: AnnotationsAction,
): AnnotationsState {
  switch (action.type) {
    case 'load-start':
      return {
        ...state,
        loaded: false,
        saved: false,
        loadingRequest: { id: action.requestId, revision: action.revision },
        error: '',
      }
    case 'load-success':
      if (!ownsRequest(state.loadingRequest, action)) return state
      if (state.revision !== action.revision) return { ...state, loadingRequest: null }
      return {
        ...state,
        document: action.document,
        dirty: false,
        loaded: true,
        loadingRequest: null,
        error: '',
      }
    case 'load-failure':
      return ownsRequest(state.loadingRequest, action)
        ? { ...state, loaded: false, saved: false, loadingRequest: null, error: action.error }
        : state
    case 'edit':
      return {
        ...state,
        document: updateAnnotation(state.document, action.target, action.annotation),
        revision: state.revision + 1,
        dirty: true,
        saved: false,
        error: '',
      }
    case 'save-start':
      return {
        ...state,
        saved: false,
        savingRequest: { id: action.requestId, revision: action.revision },
        error: '',
      }
    case 'save-success':
      if (!ownsRequest(state.savingRequest, action)) return state
      if (state.revision !== action.revision) return { ...state, savingRequest: null }
      return {
        ...state,
        document: action.document,
        dirty: false,
        saved: true,
        savingRequest: null,
        error: '',
      }
    case 'save-failure':
      return ownsRequest(state.savingRequest, action)
        ? { ...state, saved: false, savingRequest: null, error: action.error }
        : state
  }
}
