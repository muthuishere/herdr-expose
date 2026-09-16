/**
 * DATA PLANE — SPEC AMENDMENT A1. WebSocket BINARY frames, big-endian, no base64.
 *
 * server -> client:
 *   0        1                9          11                    N
 *   +--------+----------------+----------+----------+----------+
 *   | type u8| seq u64 BE     | tlen u16 | target   | payload  |
 *   +--------+----------------+----------+----------+----------+
 *   type: 1=frame  2=snapshot  3=gap (payload = u64 BE bytes_dropped)
 *
 * client -> server:
 *   [type=16 u8][tlen u16][target ASCII][raw bytes]      -- NO seq field
 *
 * `target` is ASCII. `payload` is RAW ANSI bytes; it is handed to xterm's
 * write(Uint8Array) untouched — no UTF-8 decode on the hot path.
 */

export const BIN_FRAME = 1
export const BIN_SNAPSHOT = 2
export const BIN_GAP = 3
export const BIN_INPUT = 16

export interface BinFrame {
  type: typeof BIN_FRAME | typeof BIN_SNAPSHOT
  seq: number
  target: string
  payload: Uint8Array
}

export interface BinGap {
  type: typeof BIN_GAP
  seq: number
  target: string
  bytesDropped: number
}

export type BinMessage = BinFrame | BinGap

const ascii = new TextEncoder()

/** Header sizes. Server->client carries seq; client->server does not. */
const S2C_TLEN_OFFSET = 9 // 1 (type) + 8 (seq)
const C2S_TLEN_OFFSET = 1 // 1 (type)

/**
 * Decode a server->client binary message. Returns null for an unknown type or a
 * truncated buffer — a malformed frame must never take down the stream.
 *
 * NOTE: `seq` is read as a JS number via two 32-bit halves. Exact up to 2^53,
 * which a per-connection sequence will not reach.
 */
export function decodeBinary(buf: ArrayBuffer): BinMessage | null {
  if (buf.byteLength < S2C_TLEN_OFFSET + 2) return null
  const dv = new DataView(buf)
  const type = dv.getUint8(0)
  const hi = dv.getUint32(1, false)
  const lo = dv.getUint32(5, false)
  const seq = hi * 0x1_0000_0000 + lo
  const tlen = dv.getUint16(S2C_TLEN_OFFSET, false)
  const tStart = S2C_TLEN_OFFSET + 2
  const pStart = tStart + tlen
  if (buf.byteLength < pStart) return null
  const target = asciiDecode(new Uint8Array(buf, tStart, tlen))

  if (type === BIN_GAP) {
    if (buf.byteLength < pStart + 8) return null
    const ghi = dv.getUint32(pStart, false)
    const glo = dv.getUint32(pStart + 4, false)
    return { type: BIN_GAP, seq, target, bytesDropped: ghi * 0x1_0000_0000 + glo }
  }
  if (type === BIN_FRAME || type === BIN_SNAPSHOT) {
    // Subarray view, not a copy — xterm copies internally on write().
    return { type, seq, target, payload: new Uint8Array(buf, pStart) }
  }
  return null
}

/** Encode a client->server input message. One allocation, no JSON. */
export function encodeInput(target: string, bytes: Uint8Array): ArrayBuffer {
  const t = ascii.encode(target)
  const out = new Uint8Array(C2S_TLEN_OFFSET + 2 + t.length + bytes.length)
  const dv = new DataView(out.buffer)
  dv.setUint8(0, BIN_INPUT)
  dv.setUint16(C2S_TLEN_OFFSET, t.length, false)
  out.set(t, C2S_TLEN_OFFSET + 2)
  out.set(bytes, C2S_TLEN_OFFSET + 2 + t.length)
  return out.buffer
}

/** Encode a server->client message. Used by the mock server only. */
export function encodeServerBinary(
  type: number,
  seq: number,
  target: string,
  payload: Uint8Array,
): ArrayBuffer {
  const t = ascii.encode(target)
  const out = new Uint8Array(S2C_TLEN_OFFSET + 2 + t.length + payload.length)
  const dv = new DataView(out.buffer)
  dv.setUint8(0, type)
  dv.setUint32(1, Math.floor(seq / 0x1_0000_0000), false)
  dv.setUint32(5, seq >>> 0, false)
  dv.setUint16(S2C_TLEN_OFFSET, t.length, false)
  out.set(t, S2C_TLEN_OFFSET + 2)
  out.set(payload, S2C_TLEN_OFFSET + 2 + t.length)
  return out.buffer
}

export function encodeU64(n: number): Uint8Array {
  const out = new Uint8Array(8)
  new DataView(out.buffer).setUint32(0, Math.floor(n / 0x1_0000_0000), false)
  new DataView(out.buffer).setUint32(4, n >>> 0, false)
  return out
}

function asciiDecode(b: Uint8Array): string {
  let s = ''
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i])
  return s
}
