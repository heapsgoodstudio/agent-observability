import { describe, expect, it } from 'vitest';
import { renderMarkdown } from './markdown';

// Ported 1:1 from dashboard/markdown.test.mjs: every case, unchanged.

describe('renderMarkdown', () => {
  it('shifts headings to h3+', () => {
    expect(renderMarkdown('# Title')).toBe('<h3>Title</h3>');
    expect(renderMarkdown('## Sub')).toBe('<h4>Sub</h4>');
  });

  it('renders unordered and ordered lists', () => {
    expect(renderMarkdown('- a\n- b')).toBe('<ul>\n<li>a</li>\n<li>b</li>\n</ul>');
    expect(renderMarkdown('1. a\n2. b')).toBe('<ol>\n<li>a</li>\n<li>b</li>\n</ol>');
  });

  it('renders bold and inline code', () => {
    expect(renderMarkdown('**bold** and `code`')).toBe('<p><strong>bold</strong> and <code>code</code></p>');
  });

  it('fences preserve and escape content', () => {
    expect(renderMarkdown('```\n<x> & y\n```')).toBe('<pre><code>\n&lt;x&gt; &amp; y\n</code></pre>');
  });

  it('escapes raw html everywhere', () => {
    expect(renderMarkdown('<script>alert(1)</script>')).toBe('<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>');
  });

  it('closes a list on a blank line', () => {
    expect(renderMarkdown('- a\n\ntext')).toBe('<ul>\n<li>a</li>\n</ul>\n<p>text</p>');
  });

  it('lets no unescaped angle bracket survive any hostile input', () => {
    const hostile = [
      '# <img src=x onerror=alert(1)>',
      '- <script>alert(1)</script>',
      '1. <b>x</b>',
      '**<svg onload=alert(1)>**',
      '`<iframe src=//evil>`',
      '```\n</code></pre><script>alert(1)</script>\n```',
      '<a href="javascript:alert(1)">x</a>',
      '## `<style>@import url(x)</style>`',
      'text with <details open ontoggle=alert(1)>',
      '***<wbr>**`<wbr>`*',
    ];
    const allowed = /^<\/?(?:h[3-6]|ul|ol|li|p|strong|code|pre)>/;
    for (const input of hostile) {
      const out = renderMarkdown(input);
      for (let i = 0; i < out.length; i++) {
        if (out[i] === '<') expect(out.slice(i)).toMatch(allowed);
      }
    }
  });
});
