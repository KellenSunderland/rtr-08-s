import { useEffect, useLayoutEffect, useRef } from "react";
import { renderMarkdown } from "../lib/markdown";
import type { TranscriptEntry } from "../lib/sessionState";

/** Renders the markdown subset from `renderMarkdown` (links, bold, inline code) into a span. */
function RichText({ text }: { text: string }) {
  const el = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    if (el.current) el.current.innerHTML = renderMarkdown(text);
  }, [text]);
  return <span ref={el} />;
}

export function Transcript({ entries }: { entries: TranscriptEntry[] }) {
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => end.current?.scrollIntoView?.({ block: "nearest" }), [entries]);
  return (
    <section className="panel transcript" aria-label="Transcript">
      <h3>Transcript</h3>
      <div className="scroll">
        {entries.length === 0 && <p className="muted">Nothing yet.</p>}
        {entries.map((t) => (
          <p
            key={t.key}
            className={`utt utt-${t.role}${t.partial ? " partial" : ""}${t.cancelled ? " cancelled" : ""}`}
          >
            <b>{t.role === "user" ? "You" : "Argus"}:</b> <RichText text={t.text} />
            {t.partial && <i> …</i>}
            {t.cancelled && <i> (interrupted)</i>}
          </p>
        ))}
        <div ref={end} />
      </div>
    </section>
  );
}
