import js from '@eslint/js';

export default [
  { ignores: ['node_modules/'] },
  js.configs.recommended,
  {
    files: ['internal/web/static/**/*.js'],
    languageOptions: { ecmaVersion: 2024, sourceType: 'script' },
  },
  {
    // app.js is loaded as a module; theme.js runs before the first paint as a script
    files: ['internal/web/static/app.js'],
    languageOptions: { sourceType: 'module' },
  },
  {
    files: ['internal/web/static/**/*.js'],
    rules: {
      // tsc checks every name against the browser library, so ESLint needs no list of globals
      'no-undef': 'off',
      eqeqeq: ['error', 'always', { null: 'ignore' }],
      'no-implicit-globals': 'error',
      'no-shadow': 'error',
      'no-unused-vars': ['error', { args: 'after-used', caughtErrors: 'none' }],
      'no-var': 'error',
      'prefer-const': 'error',
    },
  },
];
