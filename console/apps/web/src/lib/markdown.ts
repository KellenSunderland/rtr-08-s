/**
 * Tiny markdown subset for transcript text: `**bold**`, `` `code` `` and `[label](url)`.
 * Returns an HTML string; raw `<`, `>` and `&` in the source are escaped first so only the
 * tags produced below can appear in the output.
 */
const escapeHtml = (s: string): string =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

export function renderMarkdown(src: string): string {
  return escapeHtml(src)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
}
