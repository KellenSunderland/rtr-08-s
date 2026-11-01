"""FastAPI app: internal session WS, scenario/history REST, health and metrics."""

from __future__ import annotations

import asyncio
import contextlib
import hmac
import json
import logging
import time
from collections.abc import AsyncGenerator
from typing import Annotated, Any

from fastapi import Depends, FastAPI, HTTPException, Query, Request, WebSocket
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, PlainTextResponse

from argus.conversation.session import FrameSourceFactory, Session, SessionDeps
from argus.memory.store import MemoryStore
from argus.metrics import Metrics
from argus.perception.sanitize import GLOBAL as GLOBAL_SANITIZER
from argus.protocol import STREAM_ID_RE, ErrorMessage, ScenarioEventType, ScenarioInfo
from argus.providers import registry as provider_registry
from argus.providers.registry import Providers
from argus.scenarios.base import CORE_TOOL_NAMES
from argus.scenarios.registry import PackRegistry
from argus.settings import Settings

log = logging.getLogger(__name__)
MAX_WS_TEXT = 64 * 1024


def create_app(
    settings: Settings | None = None,
    *,
    providers: Providers | None = None,
    store: MemoryStore | None = None,
    frame_source_factory: FrameSourceFactory | None = None,
) -> FastAPI:
    settings = settings or Settings.from_env()
    providers = providers or provider_registry.make_all(
        provider_registry.load_config(settings.providers_path)
    )
    store = store or MemoryStore(settings.db_path, providers.embedder, retention_s=settings.retention_s)
    packs = PackRegistry([settings.packs_dir] if settings.packs_dir else None)
    metrics = Metrics()
    deps = SessionDeps(
        providers=providers,
        packs=packs,
        store=store,
        metrics=metrics,
        gateway_url=settings.gateway_url,
        internal_token=settings.internal_token,
        frame_source_factory=frame_source_factory,
        sanitizer=GLOBAL_SANITIZER,
    )
    sessions: dict[str, Session] = {}

    async def retention_loop() -> None:
        while True:
            await asyncio.sleep(60)
            try:
                r = store.retention_job(int(time.time() * 1000), settings.retention_s)
                metrics.inc("argus_retention_deleted_total", r["events"] + r["summaries"])
            except Exception:  # noqa: BLE001
                log.exception("retention job failed")

    @contextlib.asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncGenerator[None]:
        task = asyncio.create_task(retention_loop())
        try:
            yield
        finally:
            task.cancel()
            await asyncio.gather(*[s.close() for s in list(sessions.values())], return_exceptions=True)
            store.close()

    app = FastAPI(title="Argus agent", version="0.1.0", lifespan=lifespan)
    app.state.deps = deps
    app.state.sessions = sessions
    app.state.settings = settings

    def token_ok(given: str | None, query: str | None = None) -> bool:
        given = given or query
        if not settings.internal_token:
            return True  # dev mode: `argus serve` refuses non-loopback binds without a token
        return given is not None and hmac.compare_digest(given.encode(), settings.internal_token.encode())

    async def require_token(request: Request) -> None:
        if not token_ok(request.headers.get("x-internal-token"), request.query_params.get("token")):
            raise HTTPException(401, "invalid internal token")

    @app.exception_handler(HTTPException)
    async def _http_err(_: Request, exc: HTTPException) -> JSONResponse:
        code = {400: "bad_request", 401: "unauthorized", 404: "not_found", 429: "quota"}.get(
            exc.status_code, "internal"
        )
        return JSONResponse({"code": code, "message": str(exc.detail)}, status_code=exc.status_code)

    @app.exception_handler(RequestValidationError)
    async def _val_err(_: Request, exc: RequestValidationError) -> JSONResponse:
        return JSONResponse(
            {"code": "bad_request", "message": f"{len(exc.errors())} invalid parameter(s)"}, 400
        )

    @app.get("/healthz")
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/metrics", response_class=PlainTextResponse)
    async def metrics_endpoint() -> str:
        metrics.set("argus_sessions_active", len(sessions))
        metrics.set("argus_sanitized_sentences_total", GLOBAL_SANITIZER.removed)
        return metrics.render()

    @app.get("/v1/scenarios", dependencies=[Depends(require_token)])
    async def scenarios() -> list[dict[str, Any]]:
        core = sorted(CORE_TOOL_NAMES)
        return [
            ScenarioInfo(
                name=p.name,
                description=p.description,
                event_types=[
                    ScenarioEventType(type=t.type, description=t.description) for t in p.event_types.values()
                ],
                tools=[*core, *p.tool_names()],
            ).wire()
            for p in packs.all()
        ]

    @app.get("/v1/history", dependencies=[Depends(require_token)])
    async def history(  # since_ms/until_ms: wall-clock epoch ms
        since_ms: Annotated[int, Query(ge=0)],
        until_ms: Annotated[int, Query(ge=0)],
        stream_id: Annotated[str | None, Query(max_length=64)] = None,
        q: Annotated[str | None, Query(max_length=500)] = None,
        limit: Annotated[int, Query(ge=1, le=200)] = 50,
        contains: Annotated[str | None, Query(max_length=200)] = None,
    ) -> dict[str, Any]:
        if stream_id is not None and not STREAM_ID_RE.match(stream_id):
            raise HTTPException(400, "invalid stream_id")
        if until_ms < since_ms:
            raise HTTPException(400, "until_ms < since_ms")
        r = await store.search(
            stream_id, since_ms=since_ms, until_ms=until_ms, text=q, limit=limit, contains=contains
        )
        log.info(
            "history stream=%s window=%d..%d q=%d chars contains=%d chars -> %d events %d summaries",
            stream_id, since_ms, until_ms, len(q or ""), len(contains or ""), len(r.events), len(r.summaries),
        )  # fmt: skip
        return {
            "events": [e.wire() for e in r.events],
            "summaries": [x.wire() for x in r.summaries],
            "truncated": r.truncated,
        }

    @app.websocket("/internal/sessions/{session_id}/ws")
    async def session_ws(ws: WebSocket, session_id: str) -> None:
        if (
            not token_ok(ws.headers.get("x-internal-token"), ws.query_params.get("token"))
            or session_id in sessions
            or len(session_id) > 64
        ):
            await ws.close(code=1008)
            return
        await ws.accept()
        session = Session(session_id, deps)
        sessions[session_id] = session

        async def pump() -> None:
            try:
                while (m := await session.outbox.get()) is not None:
                    await ws.send_text(json.dumps(m))
                await ws.close(code=1000)
            except Exception:  # noqa: BLE001 - peer went away
                pass

        writer = asyncio.create_task(pump())
        try:
            while not session.closed:
                m = await ws.receive()
                if m["type"] == "websocket.disconnect":
                    break
                text = m.get("text")
                if text is None:
                    await session.emit(
                        ErrorMessage(code="bad_request", message="binary messages not accepted")
                    )
                    continue
                if len(text.encode()) > MAX_WS_TEXT:
                    await session.emit(ErrorMessage(code="bad_request", message="message too large"))
                    break  # finally: session.close() flushes the error, then the pump closes the socket
                await session.handle(text)
        finally:
            await session.close()
            with contextlib.suppress(Exception):
                await asyncio.wait_for(writer, 5)
            writer.cancel()
            sessions.pop(session_id, None)

    return app
