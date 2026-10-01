try {
  if (localStorage.getItem('navlistener-theme') === 'light') {
    document.documentElement.dataset.theme = 'light'
    document.querySelector('meta[name="theme-color"]').content = '#e8eaed'
  }
} catch {}
