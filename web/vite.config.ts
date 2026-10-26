/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// Where `npm run dev` forwards API requests: the scoring engine started with
// `make run`, which listens on :8080 unless ADDR says otherwise. Set API_URL
// to point the dev server somewhere else.
const apiURL = process.env.API_URL ?? 'http://127.0.0.1:8080';

// Every top-level path the Go API serves (see internal/api/server.go). In
// production the Go binary serves both the API and this frontend from one
// origin; in development these are proxied so the frontend can use the same
// relative URLs.
const apiPaths = ['/status', '/events', '/services', '/login', '/engine', '/scoring', '/countdown', '/reload', '/blue'];

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: Object.fromEntries(apiPaths.map((path) => [path, apiURL])),
  },
  test: {
    // Component tests render into a DOM; jsdom provides one.
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
});
