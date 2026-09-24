import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const ts = require("typescript");

// Transpiles one src/ TypeScript module to CommonJS and evaluates the real code.
// Mapped specifiers win; other "@/" TypeScript modules load recursively with the
// same map (ESM .mjs helpers must be mapped); the rest resolves from node_modules.
export function loadSourceModule(path, modules = {}, cache = new Map()) {
  if (cache.has(path)) return cache.get(path).exports;
  const source = readFileSync(new URL(`../src/${path}`, import.meta.url), "utf8");
  const output = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX, esModuleInterop: true,
    },
  }).outputText;
  const container = { exports: {} };
  cache.set(path, container);
  const resolve = (specifier) => {
    if (Object.hasOwn(modules, specifier)) return modules[specifier];
    if (specifier.startsWith("@/") && !specifier.endsWith(".mjs")) {
      const base = specifier.slice(2);
      const file = [".ts", ".tsx"].map((extension) => `${base}${extension}`)
        .find((candidate) => existsSync(new URL(`../src/${candidate}`, import.meta.url)));
      if (!file) throw new Error(`unresolved source module ${specifier}`);
      return loadSourceModule(file, modules, cache);
    }
    if (specifier.startsWith("@/")) throw new Error(`map ESM helper ${specifier} before loading ${path}`);
    return require(specifier);
  };
  new Function("require", "exports", "module", output)(resolve, container.exports, container);
  return container.exports;
}

// Render-free stand-in for the React hooks the editor modules call: state is
// captured once, refs persist, and effects run only on mount()/unmount().
export function createHookRuntime() {
  const effects = [];
  const cleanups = [];
  const react = {
    useState: (initial) => {
      let value = typeof initial === "function" ? initial() : initial;
      return [value, (next) => { value = typeof next === "function" ? next(value) : next; }];
    },
    useRef: (current) => ({ current }),
    useMemo: (factory) => factory(),
    useCallback: (callback) => callback,
    useEffect: (effect) => { effects.push(effect); },
  };
  return {
    react,
    mount() {
      for (const effect of effects.splice(0)) {
        const cleanup = effect();
        if (typeof cleanup === "function") cleanups.push(cleanup);
      }
    },
    unmount() {
      for (const cleanup of cleanups.splice(0).reverse()) cleanup();
    },
  };
}
