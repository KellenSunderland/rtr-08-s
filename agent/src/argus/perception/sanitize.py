"""Strip instruction-like text from perception summaries before it can reach a ChatModel context.

On-screen text (signs, shirts, monitors) is attacker-controlled, and vision summaries may quote it.
We therefore drop any sentence that looks like an instruction to the assistant, and count how often
that happens.
"""

from __future__ import annotations

import re
import threading

_PATTERNS = [
    r"\b(ignore|disregard|forget|override|bypass)\b.{0,40}\b(previous|prior|above|all|any|earlier|system|your)\b.{0,40}"
    r"\b(instruction|instructions|prompt|prompts|rules|directions|context)\b",
    r"\byou\s+are\s+now\b",
    r"\bfrom\s+now\s+on\b.{0,40}\byou\b",
    r"^\s*(system|assistant|developer|user|human|ai)\s*:",
    r"\b(new|updated|revised)\s+(instructions?|system\s+prompt)\b",
    r"\b(do\s+not|don'?t|never)\s+(tell|inform|reveal|mention)\b.{0,30}\b(user|anyone|operator)\b",
    r"\b(act|behave|respond|reply)\s+as\s+(if\s+you\s+(are|were)|an?\b)",
    r"\bpretend\s+(to\s+be|you)\b",
    r"\byou\s+(must|should|shall|will|need\s+to|have\s+to|are\s+to)\b",
    r"^\s*(please\s+)?(tell|respond|reply|say|answer|output|print|execute|run|send|reveal|call|invoke|"
    r"disregard|ignore|forget|override|delete|email|transmit)\b[^.]{0,80}\b"
    r"(user|assistant|you|me|your|instructions?|prompt|system|tool|password|key|secret)\b",
    r"<\|[^>]{0,40}\|>|\[/?(INST|SYS)\]|</?(system|assistant|im_start|im_end)>",
    r"```",
]
_RE = [re.compile(p, re.IGNORECASE | re.DOTALL) for p in _PATTERNS]
_SPLIT = re.compile(r"(?<=[.!?])\s+|\n+")


class Sanitizer:
    """Thread-safe sanitizer with a removed-sentence counter."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.removed = 0  # sentences removed
        self.texts_changed = 0

    def sanitize(self, text: str) -> str:
        parts = [p for p in _SPLIT.split(text) if p and p.strip()]
        kept = [p.strip() for p in parts if not is_instruction_like(p)]
        dropped = len(parts) - len(kept)
        if dropped:
            with self._lock:
                self.removed += dropped
                self.texts_changed += 1
        return " ".join(kept)


def is_instruction_like(sentence: str) -> bool:
    return any(r.search(sentence) for r in _RE)


GLOBAL = Sanitizer()


def sanitize_summary(text: str) -> str:
    return GLOBAL.sanitize(text)
