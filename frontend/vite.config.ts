import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  // These are public browser values supplied by the Docker frontend stage.
  envPrefix: ['VITE_', 'PUBLIC_FLARE_'],
  server: {
    // Local dev proxies the API to the Go server (DISABLE_CSRF=true).
    proxy: {
      '/api': 'http://localhost:8095',
      '/otlp': 'http://localhost:8095'
    }
  }
});
