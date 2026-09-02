import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  build: {
    outDir: '../internal/ui/dist',
    emptyOutDir: true,
    // Vite 8 bundles with Rolldown, not Rollup. The object form of
    // `manualChunks` is gone; `codeSplitting.groups` replaces it, and a group
    // also captures the dependencies of what it matches
    // (`includeDependenciesRecursively` defaults to true).
    //
    // The `test` demands a path separator after the package name so a prefix
    // cannot swallow a sibling.
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [
            // MapLibre GL, the vector basemap renderer behind L.maplibreGL.
            // ~900kB on its own: left ungrouped it landed inside the
            // AdminLocationsView route chunk, so every edit to that view made
            // returning users re-download 1.2MB to pick up a small change.
            { name: 'maplibre', test: /[\\/]node_modules[\\/](?:maplibre-gl|@maplibre[\\/][^\\/]+)[\\/]/ },
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8090',
      '/_': 'http://127.0.0.1:8090',
    },
  },
})
