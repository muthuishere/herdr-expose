/**
 * A markdown renderer for agent prose, built to React nodes and NEVER to HTML.
 *
 * NO `dangerouslySetInnerHTML`, anywhere, for the same reason the rest of this
 * app has none: this is an untrusted agent's output being shown to whoever
 * holds the share link. Building nodes means there is no sanitiser to get
 * wrong and no sanitiser to keep up to date — the XSS surface is not
 * mitigated, it is absent. That is also why there is no markdown dependency
 * here: every library in this space renders to an HTML string and hands you
 * the sanitising problem back.
 *
 * WHAT IT SUPPORTS, and why exactly this set. Measured over a real 24MB
 * transcript from this machine (826 tool calls, 487 prose blocks):
 *
 *     inline code   278     fences    83
 *     bold          256     bullets   52
 *     headings      113     tables    33
 *     numbered      15
 *
 * So: headings, fenced code, bullets, numbered lists, tables, bold, italic,
 * inline code, links, blockquotes and rules. Not footnotes, not nested
 * blockquotes, not HTML passthrough, not reference links — none of which
 * appeared at all, and each of which is a parser branch that can only make
 * the common case slower and riskier.
 *
 * MOBILE IS THE CONSTRAINT, not a later adjustment. The measurements at 375px:
 *
 *   - prose lines run to 715 characters. They arrive UNWRAPPED (one line per
 *     paragraph) because this is the markdown source rather than a terminal's
 *     120-column render, so the browser reflows them and the hard-wrap repair
 *     the pane view needs is not needed here at all.
 *   - fenced code reaches 111 characters and 53% exceeds the ~44 a phone fits.
 *     Code must not wrap — wrapping changes what it says — so each block
 *     scrolls horizontally on its own, never the page.
 *   - table rows reach 269 characters, and 70% of them are two columns. Those
 *     stack into label/value pairs, which is readable at any width; wider ones
 *     scroll, because a 4-column table cannot be made narrow without lying
 *     about which cell belongs to which heading.
 */

import type { ReactNode } from 'react'

/** One parsed block. Deliberately flat: there is no nesting to represent. */
type Block =
  | { kind: 'p'; text: string }
  | { kind: 'h'; level: number; text: string }
  | { kind: 'code'; lang: string; lines: string[] }
  | { kind: 'ul'; items: string[] }
  | { kind: 'ol'; items: string[] }
  | { kind: 'quote'; lines: string[] }
  | { kind: 'table'; head: string[]; rows: string[][] }
  | { kind: 'rule' }

const H = /^(#{1,6})\s+(.*)$/
const FENCE = /^\s*```+\s*([\w+-]*)\s*$/
const BULLET = /^\s*[-*+]\s+(.*)$/
const NUM = /^\s*\d+[.)]\s+(.*)$/
const QUOTE = /^\s*>\s?(.*)$/
const RULE = /^\s*(?:---+|\*\*\*+|___+)\s*$/
const TABLE_ROW = /^\s*\|(.+)\|\s*$/
/** The `|---|:--:|` separator that makes the line above it a header. */
const TABLE_SEP = /^\s*\|[\s|:-]+\|\s*$/

function cells(row: string): string[] {
  const m = TABLE_ROW.exec(row)
  if (!m) return []
  return m[1].split('|').map((c) => c.trim())
}

/** Split markdown into blocks. One pass, no lookbehind beyond the next line. */
export function parseMarkdown(src: string): Block[] {
  const lines = src.replace(/\r\n?/g, '\n').split('\n')
  const out: Block[] = []
  let i = 0

  while (i < lines.length) {
    const line = lines[i]

    // A fence wins over everything: inside one, nothing else is markup. This
    // is checked FIRST for that reason — a `# comment` or a `|` in a shell
    // pipeline must not become a heading or a table.
    const fence = FENCE.exec(line)
    if (fence) {
      const lang = fence[1] ?? ''
      const body: string[] = []
      i++
      while (i < lines.length && !FENCE.test(lines[i])) body.push(lines[i++])
      if (i < lines.length) i++ // the closing fence
      out.push({ kind: 'code', lang, lines: body })
      continue
    }

    if (line.trim() === '') {
      i++
      continue
    }

    if (RULE.test(line)) {
      out.push({ kind: 'rule' })
      i++
      continue
    }

    const h = H.exec(line)
    if (h) {
      out.push({ kind: 'h', level: h[1].length, text: h[2].trim() })
      i++
      continue
    }

    // A table needs the separator row to be a table at all. Without it a line
    // starting with `|` is just prose that happens to contain a pipe, and
    // treating it as a table silently eats the text.
    if (TABLE_ROW.test(line) && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1])) {
      const head = cells(line)
      i += 2
      const rows: string[][] = []
      while (i < lines.length && TABLE_ROW.test(lines[i])) rows.push(cells(lines[i++]))
      out.push({ kind: 'table', head, rows })
      continue
    }

    if (QUOTE.test(line)) {
      const body: string[] = []
      while (i < lines.length && QUOTE.test(lines[i])) {
        body.push((QUOTE.exec(lines[i]) as RegExpExecArray)[1])
        i++
      }
      out.push({ kind: 'quote', lines: body })
      continue
    }

    if (BULLET.test(line)) {
      const items: string[] = []
      while (i < lines.length && BULLET.test(lines[i])) {
        items.push((BULLET.exec(lines[i]) as RegExpExecArray)[1])
        i++
        // A wrapped continuation line is folded into the item it belongs to.
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !BULLET.test(lines[i]) && !NUM.test(lines[i])) {
          items[items.length - 1] += ' ' + lines[i].trim()
          i++
        }
      }
      out.push({ kind: 'ul', items })
      continue
    }

    if (NUM.test(line)) {
      const items: string[] = []
      while (i < lines.length && NUM.test(lines[i])) {
        items.push((NUM.exec(lines[i]) as RegExpExecArray)[1])
        i++
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !BULLET.test(lines[i]) && !NUM.test(lines[i])) {
          items[items.length - 1] += ' ' + lines[i].trim()
          i++
        }
      }
      out.push({ kind: 'ol', items })
      continue
    }

    // A paragraph runs to the next blank line or the next block opener. The
    // lines are JOINED, which is what makes a hard-wrapped paragraph reflow
    // instead of breaking wherever the writer's editor happened to break it.
    const para: string[] = []
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !FENCE.test(lines[i]) &&
      !H.test(lines[i]) &&
      !RULE.test(lines[i]) &&
      !BULLET.test(lines[i]) &&
      !NUM.test(lines[i]) &&
      !QUOTE.test(lines[i])
    ) {
      para.push(lines[i].trim())
      i++
    }
    if (para.length) out.push({ kind: 'p', text: para.join(' ') })
  }
  return out
}

/**
 * Inline markup: `code`, **bold**, *italic*, [text](url).
 *
 * Code is matched FIRST and its contents are never re-scanned, because
 * `**` inside a backtick span is two asterisks and nothing else.
 *
 * Bold and italic ARE re-scanned, one level down, because agents write
 * ``**`some-identifier` exists**`` constantly — a code span inside bold. Not
 * recursing renders the backticks as literal characters, which is how this was
 * caught: reading the rendered output on a phone and seeing
 * `` `gemini-robotics-er-2-streaming-preview` `` with its quotes showing.
 * The recursion terminates because the bold and italic patterns exclude their
 * own delimiters, so the inner text can never be bold or italic again.
 */
// Bold admits a LONE `*` in its content (`*(?!\*)`), so that *italic*
// nested inside **bold** still matches as bold and the recursion can then
// find the italic. Without that the bold alternative fails, the italic one
// matches across the wrong boundaries, and the reader gets stray asterisks
// with the emphasis in the wrong places.
const INLINE_SRC =
  "(`[^`\\n]+`|\\*\\*(?:[^*\\n]|\\*(?!\\*))+\\*\\*|\\*[^*\\n]+\\*|_[^_\\n]+_|\\[[^\\]\\n]+\\]\\([^)\\s]+\\))"

/** Only these schemes. A `javascript:` href is a script injection with a hat on. */
function safeHref(url: string): string | null {
  const u = url.trim()
  return /^(https?:\/\/|mailto:)/i.test(u) ? u : null
}

export function renderInlineMarkdown(text: string, keyBase = 'i'): ReactNode {
  // Fast path: most lines carry no markup at all, and this runs per block.
  if (!/[`*_[]/.test(text)) return text

  // A FRESH regex per call, not a shared module-level one.
  //
  // This function recurses into bold and italic, and a `/g` regex carries
  // `lastIndex` as mutable state on the object itself. Sharing one instance
  // means the inner call rewinds and advances the OUTER loop's cursor, which
  // is an infinite loop, not a wrong render: it crashed the page outright the
  // first time it ran against real prose containing code inside bold. The
  // fast path above returns before this for most lines, so the per-call
  // allocation is not on the hot path.
  const re = new RegExp(INLINE_SRC, 'g')
  const out: ReactNode[] = []
  let last = 0
  let m: RegExpExecArray | null
  let key = 0
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const tok = m[0]
    const k = `${keyBase}${key++}`
    if (tok.startsWith('`')) {
      out.push(<code key={k}>{tok.slice(1, -1)}</code>)
    } else if (tok.startsWith('**')) {
      out.push(<strong key={k}>{renderInlineMarkdown(tok.slice(2, -2), `${k}b`)}</strong>)
    } else if (tok.startsWith('[')) {
      const cut = tok.indexOf('](')
      const label = tok.slice(1, cut)
      const href = safeHref(tok.slice(cut + 2, -1))
      // An unsafe scheme renders as the TEXT it was, never as a dead link and
      // never silently dropped: the reader should see what the agent wrote.
      out.push(
        href ? (
          <a key={k} href={href} target="_blank" rel="noreferrer noopener">
            {label}
          </a>
        ) : (
          <span key={k}>{tok}</span>
        ),
      )
    } else {
      out.push(<em key={k}>{renderInlineMarkdown(tok.slice(1, -1), `${k}e`)}</em>)
    }
    last = m.index + tok.length
  }
  if (last === 0) return text
  if (last < text.length) out.push(text.slice(last))
  return out
}

/** Two-column tables stack; anything wider scrolls. See the file comment. */
const STACK_MAX_COLUMNS = 2

function Table({ head, rows }: { head: string[]; rows: string[][] }) {
  if (head.length <= STACK_MAX_COLUMNS && head.length > 0) {
    return (
      <dl className="md-pairs">
        {rows.map((r, ri) => (
          <div className="md-pair" key={ri}>
            <dt>{renderInlineMarkdown(r[0] ?? '', `dt${ri}`)}</dt>
            <dd>{renderInlineMarkdown(r[1] ?? '', `dd${ri}`)}</dd>
          </div>
        ))}
      </dl>
    )
  }
  return (
    <div className="md-scroll">
      <table className="md-table">
        <thead>
          <tr>
            {head.map((h, hi) => (
              <th key={hi}>{renderInlineMarkdown(h, `th${hi}`)}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, ri) => (
            <tr key={ri}>
              {r.map((c, ci) => (
                <td key={ci}>{renderInlineMarkdown(c, `td${ri}_${ci}`)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function Markdown({ source }: { source: string }) {
  const blocks = parseMarkdown(source)
  return (
    <div className="md">
      {blocks.map((b, i) => {
        switch (b.kind) {
          case 'h': {
            // Capped at h3 for SIZE, not for semantics: a document outline is
            // irrelevant inside a chat bubble, and an `# h1` rendered at h1
            // scale on a 375px screen is a full-width shout.
            const Tag = (`h${Math.min(b.level + 1, 4)}`) as 'h2' | 'h3' | 'h4'
            return (
              <Tag className="md-h" key={i} data-level={b.level}>
                {renderInlineMarkdown(b.text, `h${i}`)}
              </Tag>
            )
          }
          case 'p':
            return (
              <p className="md-p" key={i}>
                {renderInlineMarkdown(b.text, `p${i}`)}
              </p>
            )
          case 'code':
            return (
              <div className="md-scroll" key={i}>
                <pre className="md-code">
                  <code>{b.lines.join('\n')}</code>
                </pre>
              </div>
            )
          case 'ul':
            return (
              <ul className="md-ul" key={i}>
                {b.items.map((it, j) => (
                  <li key={j}>{renderInlineMarkdown(it, `u${i}_${j}`)}</li>
                ))}
              </ul>
            )
          case 'ol':
            return (
              <ol className="md-ol" key={i}>
                {b.items.map((it, j) => (
                  <li key={j}>{renderInlineMarkdown(it, `o${i}_${j}`)}</li>
                ))}
              </ol>
            )
          case 'quote':
            return (
              <blockquote className="md-quote" key={i}>
                {renderInlineMarkdown(b.lines.join(' '), `q${i}`)}
              </blockquote>
            )
          case 'table':
            return <Table key={i} head={b.head} rows={b.rows} />
          case 'rule':
            return <hr className="md-rule" key={i} />
        }
      })}
    </div>
  )
}
