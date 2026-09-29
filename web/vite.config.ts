import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": "/src" } },
  build: { outDir: "dist", emptyOutDir: true },
  // `npm run dev` talks to a console on localhost:8080 (e.g. kubectl port-forward).
  server: { proxy: { "/api": "http://localhost:8080", "/auth": "http://localhost:8080" } },
});
