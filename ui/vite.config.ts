import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
export default defineConfig({ plugins: [react()], resolve: { dedupe: ['react', 'react-dom'] }, optimizeDeps: { entries: ['index.html', 'test/browser.html'] }, build: { copyPublicDir: false, lib: { entry: 'src/index.ts', formats: ['es'], fileName: 'index', cssFileName: 'style' }, rollupOptions: { external: ['react', 'react-dom', 'react-dom/client', 'react/jsx-runtime'] } }, test: { include: ['test/**/*.test.ts'] } });
