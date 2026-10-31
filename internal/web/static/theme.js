// Runs before the first paint, so a stored choice never flashes the default look.
(() => {
  const root = document.documentElement;
  const systemDark = matchMedia('(prefers-color-scheme: dark)');
  window.applyTheme = () => {
    // "fancy" is the former name of the modern theme
    const theme = localStorage.getItem('theme');
    root.dataset.theme = theme === 'modern' || theme === 'fancy' ? 'modern' : 'classic';
    const mode = localStorage.getItem('mode') || 'auto';
    root.dataset.mode = mode === 'auto' ? (systemDark.matches ? 'dark' : 'light') : mode;
  };
  systemDark.addEventListener('change', window.applyTheme);
  window.applyTheme();
})();
