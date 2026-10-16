import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// `npm run dev` proxies the API to a local `zerowatch --role=all` (:8080);
// `npm run dev:mock` proxies to dev/mock-api.mjs (:8787) for work without Go.
export default defineConfig(({ mode }) => {
  const target = process.env.ZW_API ?? (mode === "mock" ? "http://localhost:8787" : "http://localhost:8080");
  return {
    plugins: [react(), tailwindcss()],
    resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
    build: { outDir: "dist", emptyOutDir: true, assetsDir: "assets", target: "es2022" },
    server: {
      proxy: {
        "/api": { target, changeOrigin: true },
        "/healthz": { target, changeOrigin: true },
      },
    },
  };
});
