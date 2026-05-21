// Thin wrapper around fetch for the /api/kiosk/* endpoints. Centralizes
// JSON encoding/decoding and error handling so callers can `await api.post(...)`.
//
// Auth: the kiosk-flow endpoints are anonymous, but the admin-only ones
// (/integrity, /items/import, /items/{id}/adjust, /items.csv,
// /transactions.csv) gate on requireAdmin, which reads re.Auth from the
// Authorization header. The PocketBase JS SDK holds the admin token in
// MemoryAuthStore (see lib/pb.ts) — we attach it here so plain fetch calls
// don't have to know about that.
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

async function request<T>(method: string, url: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { ...authHeaders() }
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  const res = await fetch(url, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
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
export async function download(url: string, filename?: string): Promise<void> {
  const res = await fetch(url, { headers: authHeaders() })
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
  const a = document.createElement('a')
  a.href = objectUrl
  a.download = filename || filenameFromResponse(res) || 'download'
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(objectUrl)
}

function filenameFromResponse(res: Response): string | null {
  const cd = res.headers.get('Content-Disposition') || ''
  const m = cd.match(/filename="?([^"]+)"?/i)
  return m ? m[1] : null
}

export const api = {
  get: <T>(url: string) => request<T>('GET', url),
  post: <T>(url: string, body?: unknown) => request<T>('POST', url, body),
  patch: <T>(url: string, body?: unknown) => request<T>('PATCH', url, body),
  delete: <T>(url: string) => request<T>('DELETE', url),
}
