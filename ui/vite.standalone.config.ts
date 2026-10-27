import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
export default defineConfig({ plugins: [react()], define: { 'process.env.NODE_ENV': JSON.stringify('production') }, build: { copyPublicDir: false, emptyOutDir: false, lib: { entry: 'src/standalone.ts', name: 'AgentWorkflow', formats: ['iife'], fileName: () => 'standalone.js', cssFileName: 'style' } } });
