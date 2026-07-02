import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The frontend talks to the Go backend on :8080. In dev we proxy /api and /ws so the
// browser only ever sees one origin (avoids CORS and keeps the WS same-origin).
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: true },
      "/healthz": { target: "http://localhost:8080", changeOrigin: true },
      "/ws": { target: "ws://localhost:8080", ws: true },
    },
  },
});
