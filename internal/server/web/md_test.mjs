import pw from '/opt/homebrew/lib/node_modules/@playwright/mcp/node_modules/playwright/index.js';
import { readFileSync } from 'fs';
const src = readFileSync('internal/server/web/md.js', 'utf8');
const b = await pw.chromium.launch({ channel: 'chrome', headless: true });
const p = await b.newPage();
await p.setContent('<div id=out></div>');
await p.addScriptTag({ content: src });

const cases = [
  // [name, input, must contain, must NOT contain]
  ['escapes html',        '<script>alert(1)</script>',      ['&lt;script&gt;'], ['<script>']],
  ['escapes img onerror', '<img src=x onerror=alert(1)>',    ['&lt;img'],        ['<img']],
  ['bold',                'a **bold** b',                    ['<strong>bold</strong>'], []],
  ['italic',              'a *it* b',                        ['<em>it</em>'],    []],
  ['inline code',         'use `foo()` here',                ['<code>foo()</code>'], []],
  ['code not parsed',     'text `**not bold**` end',         ['<code>**not bold**</code>'], ['<strong>']],
  ['fenced code',         '```go\nfunc x() {}\n```',         ['<pre><code class="lang-go">', 'func x()'], []],
  ['fenced escapes',      '```\n<b>raw</b>\n```',            ['&lt;b&gt;raw&lt;/b&gt;'], ['<b>raw']],
  ['heading',             '## Title',                        ['<h2>Title</h2>'], []],
  ['ul',                  '- one\n- two',                    ['<ul>', '<li>one</li>'], []],
  ['ol',                  '1. one\n2. two',                  ['<ol>', '<li>one</li>'], []],
  ['table',               '| a | b |\n|---|---|\n| 1 | 2 |', ['<table>', '<th>a</th>', '<td>1</td>'], []],
  ['blockquote',          '> quoted',                        ['<blockquote>', 'quoted'], []],
  ['hr',                  '---',                             ['<hr>'], []],
  ['http link',           '[x](https://example.com)',        ['<a href="https://example.com"'], []],
  ['javascript link',     '[x](javascript:alert(1))',        ['[x](javascript:alert(1))'], ['<a href']],
  ['data link',           '[x](data:text/html,<b>)',         [], ['<a href']],
  ['strikethrough',       '~~gone~~',                        ['<del>gone</del>'], []],
  ['paragraph breaks',    'line one\nline two',              ['<br>'], []],
  ['cjk',                 '**中文加粗**',                     ['<strong>中文加粗</strong>'], []],
  ['empty',               '',                                [], []],
  // The code-span placeholder is a private-use codepoint; input containing it
  // must not be able to pull a span out of the table.
  ['sentinel in input',   'a \uE000 0 \uE000 b',              ['\uE000'], ['<code>']],
  ['sentinel with code',  '`x` and \uE0000\uE000',            ['<code>x</code>'], []],
];

let fails = 0;
for (const [name, input, must, mustNot] of cases) {
  const html = await p.evaluate(t => window.renderMarkdown(t), input);
  const missing = must.filter(m => !html.includes(m));
  const present = mustNot.filter(m => html.includes(m));
  if (missing.length || present.length) {
    fails++;
    console.log(`FAIL ${name}`);
    if (missing.length) console.log(`     missing: ${JSON.stringify(missing)}`);
    if (present.length) console.log(`     leaked : ${JSON.stringify(present)}`);
    console.log(`     got    : ${html.slice(0, 160)}`);
  }
}
// The critical property: no input can produce an executable element.
const attacks = [
  '<script>x</script>', '<img src=x onerror=y>', '<iframe src=z>',
  '[a](javascript:x)', '<svg onload=x>', '`<script>x</script>`',
  '<a href="javascript:x">click</a>', '**<script>x</script>**',
];
// Checked against the DOM rather than the string: escaped text may legitimately
// contain "onerror=" as characters. What matters is whether the browser builds a
// dangerous element or attribute from it.
for (const a of attacks) {
  const bad = await p.evaluate(t => {
    const host = document.getElementById('out');
    host.innerHTML = window.renderMarkdown(t);
    const dangerous = host.querySelectorAll('script,iframe,svg,img,object,embed,link,style');
    const handlers = [...host.querySelectorAll('*')].filter(el =>
      [...el.attributes].some(at => /^on/i.test(at.name)));
    const badHrefs = [...host.querySelectorAll('a[href]')].filter(el =>
      !/^https?:$/i.test(new URL(el.href, location.href).protocol));
    return { tags: [...dangerous].map(e => e.tagName), handlers: handlers.length, badHrefs: badHrefs.length };
  }, a);
  if (bad.tags.length || bad.handlers || bad.badHrefs) {
    fails++; console.log(`FAIL xss: ${a}\n     ${JSON.stringify(bad)}`);
  }
}
console.log(fails ? `\n${fails} failures` : `\nall ${cases.length + attacks.length} checks passed`);
await b.close();
process.exit(fails ? 1 : 0);
