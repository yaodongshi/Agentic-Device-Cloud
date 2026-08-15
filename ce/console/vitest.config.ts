import { defineConfig, mergeConfig } from 'vitest/config'
import viteConfig from './vite.config'

// Reuse the app's vite config (alias, vue plugin) and run tests in jsdom.
// Colocated *.test.ts files under src/ are picked up; lint and typecheck
// cover them through the existing scripts.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      environment: 'jsdom',
      include: ['src/**/*.test.ts'],
      restoreMocks: true,
      server: {
        deps: {
          inline: ['element-plus'],
        },
      },
    },
  }),
)
