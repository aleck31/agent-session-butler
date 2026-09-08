// Minimal Markdown renderer for transcript text. Deliberately small and
// dependency-free: the UI is embedded in the binary and must work offline.
//
// Security: session text is arbitrary — an agent may have pasted a web page into
// it — so everything is HTML-escaped first and only the tags produced below can
// ever appear. Never change this to interpolate raw input.
(function () {
  'use strict';

  // Code spans are lifted out before inline markup is applied and put back after.
  // The placeholder must be something no input can contain: a private-use
  // codepoint, kept printable so the file stays text and diffable.
  const SENTINEL = '\uE000';
  const SENTINEL_RE = /\uE000(\d+)\uE000/g;

  function esc(s) {
    return s.replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
  }

  // Only http(s) links become anchors; anything else (javascript:, data:) stays
  // as plain text.
  function safeHref(url) {
    return /^https?:\/\//i.test(url.trim()) ? url.trim() : null;
  }

  // Inline formatting, applied to already-escaped text. Code spans are pulled out
  // first so their contents are not treated as markup.
  function inline(text) {
    const spans = [];
    text = text.replace(/`([^`]+)`/g, (_, code) => {
      spans.push('<code>' + code + '</code>');
      return SENTINEL + (spans.length - 1) + SENTINEL;
    });

    text = text
      .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, label, url) => {
        const href = safeHref(url);
        return href ? `<a href="${href}" target="_blank" rel="noopener noreferrer">${label}</a>` : m;
      })
      .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
      .replace(/(^|[\s(])\*([^*\n]+)\*/g, '$1<em>$2</em>')
      .replace(/(^|[\s(])_([^_\n]+)_/g, '$1<em>$2</em>')
      .replace(/~~([^~]+)~~/g, '<del>$1</del>');

    return text.replace(SENTINEL_RE, (_, i) => spans[+i]);
  }

  function tableRow(line, cell) {
    const cells = line.replace(/^\||\|$/g, '').split('|');
    return '<tr>' + cells.map(c => `<${cell}>${inline(esc(c.trim()))}</${cell}>`).join('') + '</tr>';
  }

  function render(src) {
    if (!src) return '';
    const lines = String(src).split('\n');
    const out = [];
    let i = 0;

    while (i < lines.length) {
      const line = lines[i];

      // Fenced code block: contents are escaped and never parsed further.
      const fence = line.match(/^\s*```(\w*)/);
      if (fence) {
        const body = [];
        i++;
        while (i < lines.length && !/^\s*```/.test(lines[i])) body.push(lines[i++]);
        i++; // closing fence
        const lang = fence[1] ? ` class="lang-${esc(fence[1])}"` : '';
        out.push(`<pre><code${lang}>${esc(body.join('\n'))}</code></pre>`);
        continue;
      }

      // Table: a header row followed by a separator row.
      if (/^\s*\|/.test(line) && i + 1 < lines.length && /^\s*\|[\s:|-]+\|?\s*$/.test(lines[i + 1])) {
        const head = tableRow(line.trim(), 'th');
        i += 2;
        const body = [];
        while (i < lines.length && /^\s*\|/.test(lines[i])) body.push(tableRow(lines[i++].trim(), 'td'));
        out.push(`<table><thead>${head}</thead><tbody>${body.join('')}</tbody></table>`);
        continue;
      }

      const heading = line.match(/^(#{1,6})\s+(.*)$/);
      if (heading) {
        const level = heading[1].length;
        out.push(`<h${level}>${inline(esc(heading[2]))}</h${level}>`);
        i++;
        continue;
      }

      if (/^\s*([-*_])\s*\1\s*\1[\s\-*_]*$/.test(line)) {
        out.push('<hr>');
        i++;
        continue;
      }

      if (/^\s*>/.test(line)) {
        const body = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*>\s?/, ''));
        out.push(`<blockquote>${render(body.join('\n'))}</blockquote>`);
        continue;
      }

      const bullet = /^\s*[-*+]\s+/;
      const number = /^\s*\d+[.)]\s+/;
      if (bullet.test(line) || number.test(line)) {
        const ordered = !bullet.test(line);
        const items = [];
        while (i < lines.length && (bullet.test(lines[i]) || number.test(lines[i]))) {
          items.push('<li>' + inline(esc(lines[i++].replace(ordered ? number : bullet, ''))) + '</li>');
        }
        const tag = ordered ? 'ol' : 'ul';
        out.push(`<${tag}>${items.join('')}</${tag}>`);
        continue;
      }

      if (!line.trim()) {
        i++;
        continue;
      }

      // Paragraph: consecutive non-blank lines that start nothing else.
      const para = [];
      while (i < lines.length && lines[i].trim() &&
             !/^\s*```/.test(lines[i]) && !/^(#{1,6})\s/.test(lines[i]) &&
             !/^\s*>/.test(lines[i]) && !bullet.test(lines[i]) && !number.test(lines[i]) &&
             !/^\s*\|/.test(lines[i])) {
        para.push(lines[i++]);
      }
      out.push('<p>' + inline(esc(para.join('\n'))).replace(/\n/g, '<br>') + '</p>');
    }

    return out.join('');
  }

  window.renderMarkdown = render;
})();
