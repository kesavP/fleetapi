<script setup>
// A handful of 24x24 line icons, drawn here so the page loads nothing from anywhere else (the CSP allows only this origin).
defineProps({ name: { type: String, required: true } })

const PATHS = {
  apps: 'M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z',
  map: 'M3 6.5 9 4l6 2.5L21 4v13.5L15 20l-6-2.5L3 20zM9 4v13.5M15 6.5V20',
  traces: 'M4 6h10M4 12h14M4 18h8M17 6h3M20 18h-3',
  logs: 'M5 4h14v16H5zM8.5 9h7M8.5 12.5h7M8.5 16h4',
  profiles: 'M12 3c1 3 4 4.5 4 8.5a4 4 0 0 1-8 0c0-1.3.4-2.2 1-3 .3 1.2 1 1.8 1.8 1.8C11 8.5 10.5 5.5 12 3zM6 21h12',
  alerts: 'M6 17V11a6 6 0 0 1 12 0v6l1.5 2h-15zM10 21a2 2 0 0 0 4 0',
  incidents: 'M12 3 2.5 20h19zM12 10v5M12 17.5v.5',
  connection: 'M9 7V3M15 7V3M7 7h10v4a5 5 0 0 1-10 0zM12 16v5',
  collapse: 'M15 5l-7 7 7 7',
  expand: 'M9 5l7 7-7 7',
  workload: 'M12 3l8 4.5v9L12 21l-8-4.5v-9zM4 7.5l8 4.5 8-4.5M12 12v9',
  database: 'M5 6c0-1.7 3.1-3 7-3s7 1.3 7 3-3.1 3-7 3-7-1.3-7-3zM5 6v12c0 1.7 3.1 3 7 3s7-1.3 7-3V6M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3',
  queue: 'M3 6h18M3 12h12M3 18h18M17 9l4 3-4 3',
  globe: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM3 12h18M12 3c2.5 2.5 3.5 5.5 3.5 9s-1 6.5-3.5 9c-2.5-2.5-3.5-5.5-3.5-9s1-6.5 3.5-9',
  zoomIn: 'M11 5v12M5 11h12',
  zoomOut: 'M5 11h12',
  fit: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
  flowRight: 'M4 12h14M13 7l5 5-5 5',
  flowDown: 'M12 4v14M7 13l5 5 5-5',
}
</script>

<template>
  <svg class="icon" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path :d="PATHS[name] || PATHS.apps" />
  </svg>
</template>
