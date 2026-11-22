import { describe, expect, it } from "vitest";
import { renderMarkdown } from "./markdown";

describe("renderMarkdown", () => {
  it("renders bold, inline code and links", () => {
    expect(renderMarkdown("a **b** `c`")).toBe("a <strong>b</strong> <code>c</code>");
    expect(renderMarkdown("[docs](https://example.com/x)")).toBe(
      '<a href="https://example.com/x" target="_blank" rel="noreferrer">docs</a>',
    );
  });
  it("escapes raw tags and ampersands", () => {
    expect(renderMarkdown("<b>x</b> & y")).toBe("&lt;b&gt;x&lt;/b&gt; &amp; y");
  });
  it("leaves unmatched markers alone", () => {
    expect(renderMarkdown("2 ** 3 and `open")).toBe("2 ** 3 and `open");
  });
});
