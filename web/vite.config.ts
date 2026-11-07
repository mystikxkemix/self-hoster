import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The build is embedded into the Go binary (internal/api/ui/dist).
// In dev, API calls are proxied to the Go server on :8080.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/api/ui/dist',
    emptyOutDir: true,
  },
  server: {
    host: true,
    port: 5173,
    proxy: { '/api': 'http://localhost:8080' },
  },
});
