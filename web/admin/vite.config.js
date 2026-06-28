import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Built assets are copied into ../../internal/adminserver/web and embedded into the broker via
// go:embed. base './' keeps asset paths relative so they resolve under the broker's "/".
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: { outDir: 'dist', emptyOutDir: true },
})
