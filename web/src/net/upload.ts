/**
 * Image upload.
 *
 * The bytes do NOT go through the prompt socket. A prompt is text an agent
 * reads; an image is a file an agent opens, and sending megabytes of base64
 * through the same socket that carries live screen frames would make a
 * screenshot compete with the thing the person is watching — and the agent
 * still could not open it afterwards.
 *
 * So the file goes to its own authenticated endpoint, the server writes it on
 * the machine that owns the pane, and what comes back is the absolute PATH.
 * The path is what the agent is given; the person sees an attachment.
 */
import { getToken } from './auth'

export const MAX_UPLOAD_BYTES = 20 * 1024 * 1024

export type Upload = {
  /** Absolute path ON THE MACHINE THAT OWNS THE PANE. */
  path: string
  mime: string
  bytes: number
  machine?: string
  /** What to show the person. The server never sees or trusts this. */
  name: string
  /** Object URL for the thumbnail; revoked when the attachment is dropped. */
  preview: string
}

export class UploadError extends Error {}

/** The session segment of `session/pane`, used only to group files on disk. */
export function sessionOf(target: string): string {
  const i = target.indexOf('/')
  return i < 0 ? target : target.slice(0, i)
}

export async function uploadImage(file: File, target: string): Promise<Upload> {
  if (file.size > MAX_UPLOAD_BYTES) {
    throw new UploadError(`that image is larger than ${MAX_UPLOAD_BYTES / 1024 / 1024}MB`)
  }
  const token = getToken()
  const q = new URLSearchParams({ session: sessionOf(target) })
  const res = await fetch(`/v1/uploads?${q}`, {
    method: 'POST',
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      // The server sniffs magic bytes and ignores this; it is here so a proxy
      // in the middle does not decide the body is text and mangle it.
      'Content-Type': file.type || 'application/octet-stream',
    },
    body: file,
  })
  let body: { path?: string; mime?: string; bytes?: number; machine?: string; error?: string } = {}
  try {
    body = await res.json()
  } catch {
    /* a proxy error page, handled below */
  }
  if (!res.ok || !body.path) {
    throw new UploadError(body.error ?? `upload failed (${res.status})`)
  }
  return {
    path: body.path,
    mime: body.mime ?? file.type,
    bytes: body.bytes ?? file.size,
    machine: body.machine,
    name: file.name || 'image',
    preview: URL.createObjectURL(file),
  }
}

/** Pull image files out of a paste or a drop, ignoring everything else. */
export function imagesFrom(dt: DataTransfer | null): File[] {
  if (!dt) return []
  const out: File[] = []
  // `items` carries a pasted screenshot, which has no entry in `files` on some
  // browsers; `files` carries a drag-drop. Both are read, then de-duplicated.
  for (const item of Array.from(dt.items ?? [])) {
    if (item.kind === 'file' && item.type.startsWith('image/')) {
      const f = item.getAsFile()
      if (f) out.push(f)
    }
  }
  for (const f of Array.from(dt.files ?? [])) {
    if (f.type.startsWith('image/') && !out.some((x) => x.name === f.name && x.size === f.size)) {
      out.push(f)
    }
  }
  return out
}
