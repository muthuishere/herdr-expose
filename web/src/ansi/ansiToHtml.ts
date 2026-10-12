/**
 * Minimal ANSI -> HTML renderer for SUMMARY tiles.
 *
 * SPEC §8: xterm.js is ONLY for the live pane. Tiles get pre-rendered HTML,
 * because N xterm instances is what kills the tab.
 *
 * Scope on purpose: SGR colour/attribute handling, plus enough cursor/erase
 * awareness not to vomit escape codes on screen. It is NOT a terminal emulator
 * — `pane.read --source visible` gives us an already-laid-out visible region,
 * so we only need to colour it, not to position it.
 */

const ESC = 0x1b

/** xterm-ish palette, tuned for the dark shell. */
const PALETTE_16 = [
  '#20242b', '#ff6b7a', '#5ddc8a', '#ffc94d',
  '#62b0ff', '#c98bff', '#4fd6d6', '#c6ccd8',
  '#4b525f', '#ff9aa5', '#8ff0af', '#ffdd8a',
  '#96ccff', '#dcb4ff', '#8ee9e9', '#ffffff',
]

let cube256: string[] | null = null
function palette256(n: number): string {
  if (n < 16) return PALETTE_16[n]
  if (!cube256) {
    const out: string[] = []
    const steps = [0, 95, 135, 175, 215, 255]
    for (let r = 0; r < 6; r++)
      for (let g = 0; g < 6; g++)
        for (let b = 0; b < 6; b++) out.push(rgb(steps[r], steps[g], steps[b]))
    for (let i = 0; i < 24; i++) {
      const v = 8 + i * 10
      out.push(rgb(v, v, v))
    }
    cube256 = out
  }
  if (n < 232 + 24) return cube256[n - 16] ?? '#c6ccd8'
  return '#c6ccd8'
}

function rgb(r: number, g: number, b: number): string {
  return `rgb(${r},${g},${b})`
}

interface Style {
  fg: string | null
  bg: string | null
  bold: boolean
  dim: boolean
  italic: boolean
  underline: boolean
  inverse: boolean
}

const BLANK: Style = {
  fg: null,
  bg: null,
  bold: false,
  dim: false,
  italic: false,
  underline: false,
  inverse: false,
}

function sameStyle(a: Style, b: Style): boolean {
  return (
    a.fg === b.fg &&
    a.bg === b.bg &&
    a.bold === b.bold &&
    a.dim === b.dim &&
    a.italic === b.italic &&
    a.underline === b.underline &&
    a.inverse === b.inverse
  )
}

function escapeHtml(s: string): string {
  let out = ''
  for (const ch of s) {
    if (ch === '&') out += '&amp;'
    else if (ch === '<') out += '&lt;'
    else if (ch === '>') out += '&gt;'
    else if (ch === '"') out += '&quot;'
    else out += ch
  }
  return out
}

function styleAttr(s: Style): string {
  let fg = s.fg
  let bg = s.bg
  if (s.inverse) {
    const t = fg ?? 'var(--term-fg)'
    fg = bg ?? 'var(--term-bg)'
    bg = t
  }
  const parts: string[] = []
  if (fg) parts.push(`color:${fg}`)
  if (bg) parts.push(`background:${bg}`)
  if (s.bold) parts.push('font-weight:700')
  if (s.dim) parts.push('opacity:.62')
  if (s.italic) parts.push('font-style:italic')
  if (s.underline) parts.push('text-decoration:underline')
  return parts.join(';')
}

function applySgr(style: Style, params: number[]): Style {
  const s = { ...style }
  if (params.length === 0) params = [0]
  for (let i = 0; i < params.length; i++) {
    const p = params[i]
    if (p === 0) {
      s.fg = null
      s.bg = null
      s.bold = s.dim = s.italic = s.underline = s.inverse = false
    } else if (p === 1) s.bold = true
    else if (p === 2) s.dim = true
    else if (p === 3) s.italic = true
    else if (p === 4) s.underline = true
    else if (p === 7) s.inverse = true
    else if (p === 21 || p === 22) {
      s.bold = false
      s.dim = false
    } else if (p === 23) s.italic = false
    else if (p === 24) s.underline = false
    else if (p === 27) s.inverse = false
    else if (p >= 30 && p <= 37) s.fg = PALETTE_16[p - 30]
    else if (p >= 90 && p <= 97) s.fg = PALETTE_16[p - 90 + 8]
    else if (p >= 40 && p <= 47) s.bg = PALETTE_16[p - 40]
    else if (p >= 100 && p <= 107) s.bg = PALETTE_16[p - 100 + 8]
    else if (p === 39) s.fg = null
    else if (p === 49) s.bg = null
    else if (p === 38 || p === 48) {
      const target = p === 38 ? 'fg' : 'bg'
      const mode = params[i + 1]
      if (mode === 5) {
        s[target] = palette256(params[i + 2] ?? 7)
        i += 2
      } else if (mode === 2) {
        s[target] = rgb(params[i + 2] ?? 0, params[i + 3] ?? 0, params[i + 4] ?? 0)
        i += 4
      } else i += 1
    }
  }
  return s
}

export interface AnsiRenderOptions {
  /** Keep only the last N lines (tiles are short). 0 = keep everything. */
  maxLines?: number
  /** Hard-truncate each line to this many chars, to bound DOM size. */
  maxCols?: number
}

/**
 * Render ANSI-bearing text to an HTML string of `<span>`s and newlines.
 * Output is escaped; the caller may safely use dangerouslySetInnerHTML.
 */
export function ansiToHtml(input: string, opts: AnsiRenderOptions = {}): string {
  const text = stripNonSgr(input)
  const lines = text.split('\n')
  // A `--source visible` read is a full screen, so a mostly-idle pane arrives
  // as blank rows then content. Trim the leading and trailing blanks or every
  // tile renders as dead space with three lines at the bottom.
  let lo = 0
  let hi = lines.length
  while (lo < hi && lines[lo].replace(/\x1b\[[0-9;]*m/g, '').trim() === '') lo++
  while (hi > lo && lines[hi - 1].replace(/\x1b\[[0-9;]*m/g, '').trim() === '') hi--
  const trimmed = lines.slice(lo, hi)
  const kept =
    opts.maxLines && opts.maxLines > 0 && trimmed.length > opts.maxLines
      ? trimmed.slice(trimmed.length - opts.maxLines)
      : trimmed

  // Style must carry across lines, so render the whole block in one pass.
  let style = { ...BLANK }
  let open = false
  let out = ''
  let pending = ''
  let cols = 0
  const maxCols = opts.maxCols ?? 0

  const flush = () => {
    if (!pending) return
    if (sameStyle(style, BLANK)) out += escapeHtml(pending)
    else {
      if (!open) {
        out += `<span style="${styleAttr(style)}">`
        open = true
      }
      out += escapeHtml(pending)
    }
    pending = ''
  }
  const closeSpan = () => {
    if (open) {
      out += '</span>'
      open = false
    }
  }

  for (let li = 0; li < kept.length; li++) {
    const line = kept[li]
    cols = 0
    let i = 0
    while (i < line.length) {
      const code = line.charCodeAt(i)
      if (code === ESC && line[i + 1] === '[') {
        const m = /^\x1b\[([0-9;]*)m/.exec(line.slice(i))
        if (m) {
          flush()
          closeSpan()
          const params = m[1] === '' ? [0] : m[1].split(';').map((x) => (x === '' ? 0 : +x))
          style = applySgr(style, params)
          i += m[0].length
          continue
        }
        // Non-SGR CSI already removed by stripNonSgr; skip defensively.
        const skip = /^\x1b\[[0-9;?]*[ -/]*[@-~]/.exec(line.slice(i))
        if (skip) {
          i += skip[0].length
          continue
        }
      }
      // A lone ESC that survived the strip pass is not printable text.
      if (code === ESC) {
        i++
        continue
      }
      if (maxCols && cols >= maxCols) break
      pending += line[i]
      cols++
      i++
    }
    flush()
    if (li < kept.length - 1) {
      closeSpan()
      out += '\n'
    }
  }
  flush()
  closeSpan()
  return out
}

/**
 * Remove escape sequences we do not render: non-SGR CSI, OSC, DCS, charset
 * selects, and the carriage-return overwrite trick that spinners use (we keep
 * only the last segment of a CR-joined line, which is what a terminal shows).
 */
export function stripNonSgr(input: string): string {
  let s = input
  // OSC ... BEL | ST
  s = s.replace(/\x1b\][\s\S]*?(?:\x07|\x1b\\)/g, '')
  // DCS/SOS/PM/APC ... ST
  s = s.replace(/\x1b[P^_X][\s\S]*?\x1b\\/g, '')
  // Non-SGR CSI
  s = s.replace(/\x1b\[[0-9;?]*[ -/]*([@-ln-~])/g, '')
  // Two-char escapes (charset, keypad, RI, ...)
  s = s.replace(/\x1b[()#][0-9A-Za-z]/g, '')
  s = s.replace(/\x1b[=><ME78DHc]/g, '')
  // Remaining C0 noise except \n, \t and ESC (0x1b). ESC must survive: the SGR
  // sequences we DO render are still escape-prefixed at this point, and a class
  // of \x0e-\x1f silently swallows it, turning every colour into literal
  // "[36m" text on screen.
  s = s.replace(/[\x00-\x08\x0b\x0c\x0e-\x1a\x1c-\x1f\x7f]/g, '')
  s = s.replace(/\r\n/g, '\n')
  // CR overwrite: keep the final segment of each line.
  //
  // A TRAILING CR is excluded, and that is the whole correctness of this.
  // "text\r" means "print text, then put the cursor back at column 0" — what
  // the terminal shows is `text`, because nothing came after to overwrite it.
  // Treating it as an overwrite returns the empty string instead, and a
  // captured screen ends EVERY line that way, so the entire buffer read as
  // blank. Only a CR with something after it actually overwrites.
  s = s
    .split('\n')
    .map((l) => {
      const t = l.replace(/\r+$/, '')
      return t.includes('\r') ? t.slice(t.lastIndexOf('\r') + 1) : t
    })
    .join('\n')
  return s
}

/** Plain text lines of a terminal buffer, blank-stripped. Shared by the digests. */
function plainLines(input: string): string[] {
  return stripNonSgr(input)
    .replace(/\x1b\[[0-9;]*m/g, '')
    .split('\n')
    .map((l) => l.replace(/\s+$/, ''))
    .filter((l) => l.trim().length > 0)
}

/**
 * The QUESTION line of a blocked agent's detection text.
 *
 * Unlike a terminal digest this takes the FIRST prose line, not the last: the
 * last line of a prompt is usually the option list or the cursor, and the thing
 * the human needs to read is the question at the top. Box-drawing frames and
 * pure option/bullet rows are skipped rather than shown.
 */
export function ansiQuestionLine(input: string, maxChars = 160): string {
  for (const line of plainLines(input)) {
    const t = line.replace(/^[\s│┃|>❯*•\-]+/, '').replace(/[\s│┃|]+$/, '').trim()
    if (!t) continue
    // A rule/frame row carries no words.
    if (!/[A-Za-z0-9]/.test(t)) continue
    return t.length > maxChars ? t.slice(0, maxChars - 1) + '…' : t
  }
  return ''
}

/**
 * The COMPOSER at the bottom of an agent's screen, and the status strip under
 * it. Everything matched here is the TUI talking about itself.
 *
 * WHY THIS IS NOT core.DefaultChrome, which the server already owns and which
 * clean.go rightly warns against writing twice. It is a DIFFERENT transform
 * with a different correct answer, and the input line is the proof:
 *
 *   - For a TRANSCRIPT, a caret with text after it must SURVIVE. It is the
 *     prompt the person just sent, and hiding it was the "I send it and it is
 *     not displaying" bug clean.go documents at length.
 *   - For a ONE-LINE ROW DIGEST it must GO. A row has room for one line and
 *     that line should say what the agent last said, not echo a draft back.
 *
 * Same screen, opposite answers. So this is a second rule, not a second copy.
 */
const ROW_CHROME: RegExp[] = [
  // The input line: caret with text after it, OR an empty caret. ASCII '>'
  // stays out of the class -- it is a quote marker in real output.
  /^\s*[❯›](\s|$)/u,
  // "new task? /clear to save 116.7k tokens" — a hint, not output.
  /\/clear to save\b/i,
  // The context meter and the mode banner, which the server also strips.
  /ctx\s+[\d.]+[KM]?\s+\d+%/i,
  /\b(auto mode|bypass permissions|accept edits)\b.*\(/i,
  /shift\+tab to cycle/i,
  // A status toast, which is GLYPH-LED: "✔ Update installed · Restart to
  // update", "⚠ veilpipe: 233 patterns · 2 replaced". Matched on the leading
  // glyph rather than on its words, because every agent and every plugin
  // invents its own sentence and they all draw the same tick.
  //
  // Safe HERE and nowhere else: this list is consulted only while walking the
  // last few lines of a screen looking for one worth showing. A tick in the
  // middle of real output is never tested against it.
  /^[✔✓✗⚠ℹ⚑]\s/u,
  // The running/finished timing banners.
  /·\s*[↓v]?\s*[\d.]+k?\s*tokens/i,
  /\bfor\s+(\d+h\s*)?(\d+m\s*)?\d+s\b.*·.*\bdone\b/i,
]

/** A horizontal rule: eight or more box-drawing/dash characters and nothing else. */
const RULE = /^[\s─-╿_=\-—–]{8,}$/u

/**
 * How far above the bottom a closing rule may sit and still be read as the
 * composer's frame rather than as content. The composer is always the last
 * thing on screen; a `---` in real output is not.
 */
const COMPOSER_WINDOW = 6

/** One line with its SGR codes removed, for testing against the patterns above. */
function plainOf(line: string): string {
  return stripNonSgr(line)
    .replace(/\x1b\[[0-9;]*m/g, '')
    .trim()
}

/**
 * Drop the agent's composer, and the status strip under it, off the bottom of
 * a captured screen. Everything above is left byte-for-byte alone, SGR codes
 * included, so a caller can still render it in colour.
 *
 * The composer is found STRUCTURALLY rather than by vocabulary: a TUI boxes its
 * input between horizontal rules, so the last rule near the bottom is the one
 * that closes the box and nothing from there down is output. That holds for any
 * agent that draws a box, not only the ones whose banner text we happen to
 * know; ROW_CHROME is just the mop-up for what sits above the rule.
 *
 * Both the list row and the desktop tile call this, so "where does this screen
 * stop being output" has one answer and not two.
 */
export function stripComposer(input: string): string {
  const raw = input.split('\n')
  // Only when the closing rule is genuinely at the bottom: a markdown `---` in
  // the middle of real output is content, and must not take the rest of the
  // screen with it.
  let end = raw.length
  for (let i = raw.length - 1; i >= 0 && raw.length - i <= COMPOSER_WINDOW; i--) {
    if (RULE.test(plainOf(raw[i]))) {
      end = i
      break
    }
  }
  // Then the furniture above it — the opening rule of the same box included,
  // because cutting at the closing one leaves the opening one as the new last
  // line, and a tile that ends in 119 dashes is no better than one that ends
  // in the keyboard hint.
  while (end > 0) {
    const t = plainOf(raw[end - 1])
    if (t === '' || RULE.test(t) || ROW_CHROME.some((re) => re.test(t))) {
      end--
      continue
    }
    break
  }
  return raw.slice(0, end).join('\n')
}

/**
 * Plain-text, single-line digest of a terminal buffer — for list rows.
 *
 * It used to take the literal last non-blank line, which on every Claude pane
 * on this machine was `⏵⏵ bypass permissions on (shift+tab to cycle)`. Thirty
 * rows said the same sentence about the keyboard and not one said what its
 * agent was doing.
 */
export function ansiSummaryLine(input: string, maxChars = 120): string {
  const lines = plainLines(stripComposer(input))
  const last = lines[lines.length - 1] ?? ''
  const trimmed = last.trim()
  return trimmed.length > maxChars ? trimmed.slice(0, maxChars - 1) + '…' : trimmed
}
