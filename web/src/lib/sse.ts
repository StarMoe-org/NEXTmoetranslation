"use client";

import { useEffect, useRef, useState } from "react";
import { clearSession, ensureSessionMigrated, getSessionEnvelope, subscribeSessionChanged } from "./api";
import { sameSessionIdentity, withSessionIdentityLock, type SessionEnvelope } from "./session";
import { runFetchSSE } from "./fetch-sse.mjs";

// SSE event names (mirror the Go sse package constants).
export type SSEEvent =
  | "entry.updated"
  | "entry.locale.updated"
  | "eventstory.updated"
  | "eventstory.locale.updated"
  | "lyrics.updated"
  | "sync.progress"
  | "translate.progress"
  | "content.restored"
  | "presence.snapshot"
  | "presence.joined"
  | "presence.left"
  | "sse.disconnected"
  | "sse.reconnected"
  | "sse.missed-events"
  | "gate.status"
  | "ping";

export type SSEHandler = (event: SSEEvent, data: unknown) => void;

const SSE_BASE = process.env.NEXT_PUBLIC_API_BASE
  ? process.env.NEXT_PUBLIC_API_BASE.replace(/\/api$/, "")
  : "";

const SSE_EVENTS = new Set<SSEEvent>([
  "entry.updated", "entry.locale.updated", "eventstory.updated", "eventstory.locale.updated", "lyrics.updated", "sync.progress",
  "translate.progress", "content.restored", "presence.snapshot", "presence.joined", "presence.left", "gate.status", "ping",
]);

function sessionVersion(): string {
  const envelope = getSessionEnvelope();
  return `${envelope?.epoch || ""}\u0000${envelope?.session?.token || ""}`;
}

/** useSSE subscribes to the bearer-authenticated stream while mounted. */
export function useSSE(handler: SSEHandler, enabled: boolean) {
  const handlerRef = useRef(handler);
  handlerRef.current = handler;
  const activeControllerRef = useRef<AbortController | null>(null);
  const [version, setVersion] = useState(sessionVersion);

  useEffect(() => subscribeSessionChanged(() => {
    if (activeControllerRef.current) handlerRef.current("sse.disconnected", { at: Date.now(), sessionChanged: true });
    activeControllerRef.current?.abort(new DOMException("会话已变化", "AbortError"));
    setVersion(sessionVersion());
  }), []);

  useEffect(() => {
    if (!enabled) return;
    const initial = getSessionEnvelope();
    if (!initial?.session) return;
    const controller = new AbortController();
    activeControllerRef.current = controller;

    void (async () => {
      let opened = false;
      await ensureSessionMigrated(controller.signal);
      await runFetchSSE({
        url: `${SSE_BASE}/sse`,
        headers: { "X-SSE-Presence": "1" },
        signal: controller.signal,
        getSession: (signal: AbortSignal) => withSessionIdentityLock("shared", () => {
          const current = getSessionEnvelope();
          if (!sameSessionIdentity(current, initial) || !current?.session) return null;
          return { token: current.session.token, envelope: current };
        }, signal),
        onEvent: (name: string, raw: string) => {
          if (!SSE_EVENTS.has(name as SSEEvent)) return;
          let data: unknown = raw;
          try {
            data = JSON.parse(raw);
          } catch {
            // Preserve non-JSON SSE payloads as strings.
          }
          handlerRef.current(name as SSEEvent, data);
        },
        // There is no replay log, so the interval before the first stream opens
        // must be reconciled just like any later reconnect gap.
        onOpen: ({ reconnected }: { reconnected: boolean } = { reconnected: false }) => {
          if (!opened && !reconnected) handlerRef.current("sse.missed-events", { at: Date.now(), initial: true });
          opened = true;
        },
        onDisconnect: () => handlerRef.current("sse.disconnected", { at: Date.now() }),
        onReconnect: () => handlerRef.current("sse.reconnected", { at: Date.now() }),
        onMissedEvents: () => handlerRef.current("sse.missed-events", { at: Date.now() }),
        onUnauthorized: ({ envelope }: { envelope: SessionEnvelope }) => clearSession(envelope),
      });
    })().catch(() => {
      // Reconnect failures are contained by the transport; session and unmount
      // cancellation intentionally produce no credential-bearing diagnostics.
    });

    return () => {
      controller.abort(new DOMException("SSE 已停止", "AbortError"));
      if (activeControllerRef.current === controller) activeControllerRef.current = null;
    };
  }, [enabled, version]);
}
