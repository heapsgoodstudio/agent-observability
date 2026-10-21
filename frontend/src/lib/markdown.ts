// Minimal markdown renderer for analysis reports: headings, lists, bold,
// inline code, fenced code, paragraphs. Input is untrusted model output, so
// every line is HTML-escaped before any tag this file emits is added; the
// result is safe to assign via dangerouslySetInnerHTML.
const escapeHtml = (s: string): string => s
  .replace(/&/g, '&amp;').replace(/</g, '&lt;')
  .replace(/>/g, '&gt;').replace(/"/g, '&quot;');

const inline = (s: string): string => s
  .replace(/`([^`]+)`/g, '<code>$1</code>')
  .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');

export function renderMarkdown(text: unknown): string {
  const out: string[] = [];
  let list: 'ul' | 'ol' | null = null;
  let fence = false;
  const closeList = () => {
    if (list) {
      out.push(`</${list}>`);
      list = null;
    }
  };
  for (const raw of String(text).split('\n')) {
    if (raw.trimStart().startsWith('```')) {
      closeList();
      out.push(fence ? '</code></pre>' : '<pre><code>');
      fence = !fence;
      continue;
    }
    const line = escapeHtml(raw);
    if (fence) {
      out.push(line);
      continue;
    }
    const h = line.match(/^(#{1,4})\s+(.*)$/);
    const ul = line.match(/^\s*[-*]\s+(.*)$/);
    const ol = line.match(/^\s*\d+\.\s+(.*)$/);
    if (h) {
      closeList();
      const level = h[1].length + 2;
      out.push(`<h${level}>${inline(h[2])}</h${level}>`);
    } else if (ul) {
      if (list !== 'ul') {
        closeList();
        out.push('<ul>');
        list = 'ul';
      }
      out.push(`<li>${inline(ul[1])}</li>`);
    } else if (ol) {
      if (list !== 'ol') {
        closeList();
        out.push('<ol>');
        list = 'ol';
      }
      out.push(`<li>${inline(ol[1])}</li>`);
    } else if (line.trim() === '') {
      closeList();
    } else {
      closeList();
      out.push(`<p>${inline(line)}</p>`);
    }
  }
  if (fence) out.push('</code></pre>');
  closeList();
  return out.join('\n');
}
