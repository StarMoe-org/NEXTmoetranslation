import { useEffect, useRef, useState } from "react";

// Polls the current HTML document for a changed ETag so a deployed rebuild can be
// announced without discarding unsaved editor state.
export function useAppUpdateProbe(): boolean {
  const [updateAvailable, setUpdateAvailable] = useState(false);
  const initialHTMLTagRef = useRef<string | null>(null);

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const checkForUpdate = async () => {
      try {
        const probeURL = new URL(window.location.href);
        probeURL.searchParams.set("__nexttrans_probe", String(Date.now()));
        const response = await fetch(probeURL.toString(), {
          cache: "no-store",
          headers: { Accept: "text/html" },
        });
        if (!response.ok) return;
        const etag = response.headers.get("ETag");
        if (!etag) return;
        if (initialHTMLTagRef.current === null) {
          initialHTMLTagRef.current = etag;
          window.sessionStorage.setItem("nexttrans-html-etag", etag);
        } else if (etag !== initialHTMLTagRef.current) {
          setUpdateAvailable(true);
        }
      } catch {
        // A temporary probe failure must not interrupt editing.
      } finally {
        if (!stopped) timer = setTimeout(checkForUpdate, 60_000);
      }
    };
    void checkForUpdate();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
    };
  }, []);

  return updateAvailable;
}
