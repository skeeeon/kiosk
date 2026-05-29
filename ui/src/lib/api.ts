// Thin wrapper around fetch for the /api/kiosk/* endpoints. Centralizes
// JSON encoding/decoding and error handling so callers can `await api.post(...)`.
//
// Auth: the kiosk-flow endpoints are anonymous, but the admin-only ones
// (/integrity, /{items,users,groups}/import and matching /template
// downloads, /items/{id}/adjust, /items.csv, /transactions.csv) gate on
// requireAdmin, which reads re.Auth from the Authorization header. The
// PocketBase JS SDK holds the admin token in MemoryAuthStore (see
// lib/pb.ts) — we attach it here so plain fetch calls don't have to know
// about that.
import { pb } from './pb'

export class ApiError extends Error {
  status: number
  data: unknown
  constructor(message: string, status: number, data: unknown) {
    super(message)
    this.status = status
    this.data = data
  }
}

function authHeaders(): Record<string, string> {
  const t = pb.authStore.token
  return t ? { Authorization: `Bearer ${t}` } : {}
}

// Default per-request timeout. A kiosk wired to a controller over a flaky LAN
// can otherwise leave a fetch pending forever (server hung, NAT dropped the
// socket), which leaves UI flags like `committing`/`rfidScanning` stuck true
// and the only recovery a manual reload. RFID endpoints block for the read
// window server-side, so callers pass a longer timeoutMs for those.
const DEFAULT_TIMEOUT_MS = 15000

export interface RequestOpts {
  timeoutMs?: number
}

// abortError maps a fetch rejection (timeout or network failure) to an
// ApiError so the flow's existing catch handlers surface it as a toast and
// settle their loading flags, instead of the promise hanging unresolved.
function abortError(e: unknown): ApiError {
  if (e instanceof DOMException && e.name === 'TimeoutError') {
    return new ApiError('Request timed out — please try again', 408, null)
  }
  return new ApiError(e instanceof Error ? e.message : 'Network error', 0, null)
}

async function request<T>(method: string, url: string, body?: unknown, opts?: RequestOpts): Promise<T> {
  const headers: Record<string, string> = { ...authHeaders() }
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  let res: Response
  try {
    res = await fetch(url, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS),
    })
  } catch (e) {
    throw abortError(e)
  }
  const data = res.status === 204 ? null : await res.json().catch(() => null)
  if (!res.ok) {
    const msg =
      (data && typeof data === 'object' && 'message' in data && typeof (data as { message: unknown }).message === 'string'
        ? (data as { message: string }).message
        : `HTTP ${res.status}`)
    throw new ApiError(msg, res.status, data)
  }
  return data as T
}

// download fetches a file with the admin Authorization header attached and
// triggers a browser save. Plain <a href> can't carry the bearer token, so
// admin-only download endpoints need this path instead.
// download fetches a file (admin CSV exports etc.) with a generous timeout —
// exports can be large — and surfaces stalls as ApiError instead of hanging.
const DOWNLOAD_TIMEOUT_MS = 60000

export async function download(url: string, filename?: string): Promise<void> {
  let res: Response
  try {
    res = await fetch(url, { headers: authHeaders(), signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS) })
  } catch (e) {
    throw abortError(e)
  }
  if (!res.ok) {
    const data = await res.json().catch(() => null)
    const msg =
      data && typeof data === 'object' && 'message' in data && typeof (data as { message: unknown }).message === 'string'
        ? (data as { message: string }).message
        : `HTTP ${res.status}`
    throw new ApiError(msg, res.status, data)
  }
  const blob = await res.blob()
  const objectUrl = URL.createObjectURL(blob)
  try {
    const a = document.createElement('a')
    a.href = objectUrl
    a.download = filename || filenameFromResponse(res) || 'download'
    document.body.appendChild(a)
    a.click()
    a.remove()
  } finally {
    // Revoke even if click()/DOM ops throw, so the object URL never leaks.
    URL.revokeObjectURL(objectUrl)
  }
}

function filenameFromResponse(res: Response): string | null {
  const cd = res.headers.get('Content-Disposition') || ''
  const m = cd.match(/filename="?([^"]+)"?/i)
  return m ? m[1] : null
}

export const api = {
  get: <T>(url: string, opts?: RequestOpts) => request<T>('GET', url, undefined, opts),
  post: <T>(url: string, body?: unknown, opts?: RequestOpts) => request<T>('POST', url, body, opts),
  patch: <T>(url: string, body?: unknown, opts?: RequestOpts) => request<T>('PATCH', url, body, opts),
  delete: <T>(url: string, opts?: RequestOpts) => request<T>('DELETE', url, undefined, opts),
}
