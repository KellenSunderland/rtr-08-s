# Argus gateway

Go service: stream registry + frame store (mediacore via cgo, or a pure-Go fake), REST/WS API, session relay to the agent.

    make test                 # fake media, no cgo needed
    make build && cp gateway.example.yaml gateway.yaml && make run
    make build-mediacore      # real mediacore (-tags mediacore), needs ../mediacore/build

## Config (`gateway.yaml`, see `gateway.example.yaml`)
Flags: `-config`, `-listen`, `-log-level`. Each bearer token carries its own quotas (`max_streams`,
`max_sessions`). `internal_token` guards `/internal/*`, which is reachable from loopback only.
Browsers cannot set headers on a WebSocket handshake, so tokens may also be passed as `?token=`.

## Walkthrough
    T=dev-token-change-me; H="Authorization: Bearer $T"
    curl -XPOST localhost:8080/v1/streams -H "$H" -d '{"id":"cam1","name":"Test","uri":"fake://640x480@10"}'
    curl localhost:8080/v1/streams -H "$H"
    curl -o f.jpg "localhost:8080/v1/streams/cam1/frame?max_w=320" -H "$H" -D-
    curl -o clip.zip "localhost:8080/v1/streams/cam1/clip?from_ms=0&to_ms=5000&fps=2" -H "$H"
    curl -XPOST localhost:8080/v1/sessions -H "$H" -d '{"scenario":"pets","stream_ids":["cam1"]}'
    # then ws://localhost:8080/v1/sessions/<id>/ws?token=$T ; first message: {"type":"session.start",...}
    curl "localhost:8080/v1/history?since_ms=0&until_ms=9999999999999" -H "$H"   # wall-clock ms, proxied

ts_ms in frame/clip endpoints is relative to the gateway start unless `session_id=<id>` is given
(then relative to that session's start).
