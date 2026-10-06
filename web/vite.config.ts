import { defineConfig } from "vite";
export default defineConfig({
  server: {
    proxy: {
      // Keep the browser's Host so management requests pass same-origin checks.
      "/api": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/auth": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/v1": "http://127.0.0.1:8080",
      "/v1beta": "http://127.0.0.1:8080",
    },
  },
});
