// One error shape for every API the console calls (FR-UI-003): Google's
// {error:{code,message,status,details}} envelope from the public APIs, the
// admin API's {error:"message"}, and the Kubernetes Status objects that
// the Connect gateway passes through from a cluster's API server.

export class ApiError extends Error {
  readonly httpStatus: number
  readonly status: string
  readonly details: unknown[]

  constructor(httpStatus: number, status: string, message: string, details: unknown[] = []) {
    super(message)
    this.name = 'ApiError'
    this.httpStatus = httpStatus
    this.status = status
    this.details = details
  }
}

// Canonical gRPC status names for HTTP statuses, as Google maps them.
const statusByHttp: Record<number, string> = {
  400: 'INVALID_ARGUMENT',
  401: 'UNAUTHENTICATED',
  403: 'PERMISSION_DENIED',
  404: 'NOT_FOUND',
  409: 'ALREADY_EXISTS',
  412: 'FAILED_PRECONDITION',
  429: 'RESOURCE_EXHAUSTED',
  499: 'CANCELLED',
  500: 'INTERNAL',
  501: 'UNIMPLEMENTED',
  503: 'UNAVAILABLE',
  504: 'DEADLINE_EXCEEDED',
}

function statusFor(httpStatus: number): string {
  return statusByHttp[httpStatus] ?? (httpStatus >= 500 ? 'INTERNAL' : 'UNKNOWN')
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

/** toApiError normalises an error response's status and parsed body. */
export function toApiError(httpStatus: number, body: unknown): ApiError {
  const err = isRecord(body) ? body.error : undefined
  if (isRecord(err)) {
    return new ApiError(
      httpStatus,
      typeof err.status === 'string' ? err.status : statusFor(httpStatus),
      typeof err.message === 'string' ? err.message : `HTTP ${httpStatus}`,
      Array.isArray(err.details) ? (err.details as unknown[]) : [],
    )
  }
  if (typeof err === 'string') {
    return new ApiError(httpStatus, statusFor(httpStatus), err)
  }
  if (isRecord(body) && body.kind === 'Status' && typeof body.message === 'string') {
    return new ApiError(httpStatus, statusFor(httpStatus), body.message)
  }
  const text = typeof body === 'string' ? body.trim() : ''
  return new ApiError(httpStatus, statusFor(httpStatus), text || `HTTP ${httpStatus}`)
}

/** networkError wraps a fetch that never got a response. */
export function networkError(cause: unknown): ApiError {
  const why = cause instanceof Error ? cause.message : String(cause)
  return new ApiError(
    0,
    'UNAVAILABLE',
    `Cannot reach the emulator (${why}). Is the Instance running?`,
  )
}

/**
 * shouldRetry is the queries' retry policy: never on a 4xx answer, up to
 * twice on a 5xx or a network error.
 */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.httpStatus >= 400 && error.httpStatus < 500) {
    return false
  }
  return failureCount < 2
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

export interface FieldViolation {
  field: string
  description: string
}

/** fieldViolations returns the google.rpc.BadRequest field errors in details. */
export function fieldViolations(error: unknown): FieldViolation[] {
  if (!(error instanceof ApiError)) return []
  const out: FieldViolation[] = []
  for (const d of error.details) {
    if (!isRecord(d) || d['@type'] !== 'type.googleapis.com/google.rpc.BadRequest') continue
    const list = d.fieldViolations
    if (!Array.isArray(list)) continue
    for (const v of list as unknown[]) {
      if (isRecord(v) && typeof v.field === 'string' && typeof v.description === 'string') {
        out.push({ field: v.field, description: v.description })
      }
    }
  }
  return out
}
