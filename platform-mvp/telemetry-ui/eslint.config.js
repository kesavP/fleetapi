import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'

export default [
  { ignores: ['dist/', 'coverage/', 'node_modules/'] },
  js.configs.recommended,
  // The preset that catches mistakes. Layout (attribute line breaks and the like) is a formatter's job.
  ...pluginVue.configs['flat/essential'],
  {
    // the three __APP_…__ names are defined by vite.config.js at build time (see src/version.js)
    languageOptions: { globals: { ...globals.browser, ...globals.node, __APP_VERSION__: 'readonly', __APP_COMMIT__: 'readonly', __APP_BUILT__: 'readonly' } },
    rules: { 'vue/multi-word-component-names': 'off' },
  },
]
