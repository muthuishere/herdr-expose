/**
 * The web tests. `npm test`.
 *
 * There was no JS test harness in this repo, and the cost of that showed up
 * twice in one afternoon: stripComposer shipped to a browser once leaving a
 * row of 119 dashes, and once cutting every pane's whole screen away. Both
 * were a two-line assertion away from being caught before the build.
 *
 * It takes no new dependency. esbuild is already here (vite's own), and the
 * assertions are node's. Bundling is what lets a .ts module be imported at all.
 */

import { strict as assert } from 'node:assert'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const out = join(mkdtempSync(join(tmpdir(), 'herdr-web-test-')), 'bundle.mjs')

execFileSync(
  join(here, '..', 'node_modules', '.bin', 'esbuild'),
  [join(here, '..', 'src', 'ansi', 'ansiToHtml.ts'), '--bundle', '--format=esm', `--outfile=${out}`],
  { stdio: ['ignore', 'ignore', 'inherit'] },
)

const { stripComposer, ansiSummaryLine } = await import(out)

let failed = 0
function test(name, fn) {
  try {
    fn()
    console.log(`  ok   ${name}`)
  } catch (err) {
    failed++
    console.log(`  FAIL ${name}\n       ${err.message.split('\n')[0]}`)
  }
}

/* --- stripComposer ---------------------------------------------------- */

const pane = readFileSync(join(here, 'fixtures-claude-pane.ansi'), 'utf8')

console.log('stripComposer')

test('cuts the composer and the status strip off a real screen shape', () => {
  const kept = stripComposer(pane).split('\n')
  // Four content lines survive; the rule-boxed input and everything under it
  // goes, and so does the "/clear to save" hint above it.
  assert.equal(kept.length, 4)
  assert.match(kept[0], /Background:/)
  assert.match(kept[3], /turn\.complete/)
})

test('keeps the SGR codes on the lines it keeps', () => {
  // The tile renders this in colour. Stripping the composer must not also
  // strip the palette.
  assert.ok(stripComposer(pane).includes('\x1b[38;2;153;153;153m'))
})

test('a CR-terminated line is not mistaken for a blank one', () => {
  // stripNonSgr implements the spinner's carriage-return overwrite, so a line
  // ENDING in CR once collapsed to '' and the whole buffer read as empty.
  assert.equal(stripComposer('hello\r\nworld\r').split('\n').length, 2)
})

test('leaves a screen with no composer completely alone', () => {
  const shell = 'npm run build\r\n  built in 291ms\r\n$ \r'
  assert.equal(stripComposer(shell), shell.replace(/\r$/, '\r'))
})

test('a markdown rule in the middle of output is content, not a composer', () => {
  // Only a rule NEAR THE BOTTOM closes a box. One with ten lines of prose
  // under it is a horizontal rule in something the agent printed.
  const text = ['intro', '-'.repeat(40), ...Array.from({ length: 10 }, (_, i) => `body ${i}`)].join('\n')
  assert.equal(stripComposer(text).split('\n').length, 12)
})

test('does not run off the end of an empty buffer', () => {
  assert.equal(stripComposer(''), '')
  assert.equal(stripComposer('\n\n\n'), '')
})

/* --- ansiSummaryLine --------------------------------------------------- */

console.log('ansiSummaryLine')

test('answers with what the agent said, not with the keyboard hint', () => {
  const line = ansiSummaryLine(pane)
  assert.match(line, /turn\.complete/)
  assert.doesNotMatch(line, /bypass permissions|shift\+tab|ctx 114K/)
})

test('never answers with a bare rule', () => {
  assert.doesNotMatch(ansiSummaryLine(pane), /^[\s─-╿_=\-]+$/u)
})

test('never answers with the input caret', () => {
  assert.doesNotMatch(ansiSummaryLine(pane), /^[❯›]/u)
  assert.equal(ansiSummaryLine('output here\r\n❯\r'), 'output here')
})

test('truncates to the asked-for width with an ellipsis', () => {
  const long = 'x'.repeat(300)
  const got = ansiSummaryLine(long, 40)
  assert.equal(got.length, 40)
  assert.ok(got.endsWith('…'))
})

test('an empty buffer yields an empty line, never undefined', () => {
  assert.equal(ansiSummaryLine(''), '')
})

console.log(failed === 0 ? '\nall passed' : `\n${failed} FAILED`)
process.exit(failed === 0 ? 0 : 1)
