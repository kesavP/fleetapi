<script setup>
import { computed } from 'vue'
import { H, W, chartModel } from './timeSeries'

// A small line chart of one or more series over a time range, with the moments that matter marked on it and an optional limit line. It draws what it is
// given and asks for nothing. For someone who cannot see the picture the same facts are in text: a label for the whole chart, the latest and highest value
// of each series, and the list of marked moments. It scales with the page (the lines keep their width) and takes its colours from the page's theme.
const props = defineProps({
  label: { type: String, required: true }, // what the chart shows: read out as its name
  series: { type: Array, default: () => [] }, // [{name, points: [{t, v}], tone?: 'a' | 'b' | 'c', dashed?: boolean}]
  markers: { type: Array, default: () => [] }, // [{t, kind, label}]
  from: { type: Number, required: true }, // epoch ms
  to: { type: Number, required: true },
  limit: { type: Object, default: null }, // {value, label}
  format: { type: Function, default: (v) => String(Math.round(v * 100) / 100) },
})

const model = computed(() => chartModel({ series: props.series, markers: props.markers, from: props.from, to: props.to, limit: props.limit }))
const clock = (t) => {
  const d = new Date(t)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
const spanText = computed(() => `${clock(props.from)} – ${clock(props.to)}`)
const tone = (i) => props.series[i].tone || 'abc'[i % 3]
const summary = computed(() => {
  const parts = model.value.stats.filter((s) => s.count).map((s) => `${s.name}: now ${props.format(s.latest)}, highest ${props.format(s.max)}`)
  if (props.limit && model.value.limitY !== null) parts.push(`${props.limit.label || 'limit'} ${props.format(props.limit.value)}`)
  return parts.join('; ')
})
</script>

<template>
  <figure class="ts">
    <p v-if="model.empty" class="help" data-testid="ts-empty">{{ label }}: no data in this window.</p>
    <template v-else>
      <svg class="ts-svg" :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" role="img" :aria-label="`${label}. ${spanText}. ${summary}`">
        <line v-for="g in [0, 0.5, 1]" :key="g" class="grid" x1="0" :x2="W" :y1="g * H" :y2="g * H" vector-effect="non-scaling-stroke" />
        <line v-if="model.limitY !== null" class="limit" x1="0" :x2="W" :y1="model.limitY" :y2="model.limitY" vector-effect="non-scaling-stroke" data-testid="ts-limit" />
        <line v-for="(m, i) in model.marks" :key="'m' + i" class="mark" :class="m.kind" :x1="m.x" :x2="m.x" y1="0" :y2="H" vector-effect="non-scaling-stroke" data-testid="ts-mark" />
        <g v-for="(l, i) in model.lines" :key="l.name" class="line" :class="[tone(i), { dashed: series[i].dashed }]">
          <path v-if="l.d" :d="l.d" fill="none" vector-effect="non-scaling-stroke" data-testid="ts-line" />
          <path v-for="(p, j) in l.dots" :key="j" :d="`M${p.x} ${p.y}L${p.x} ${p.y}`" fill="none" stroke-linecap="round" vector-effect="non-scaling-stroke" data-testid="ts-dot" />
        </g>
      </svg>
      <figcaption class="ts-axis"><span>{{ spanText }}</span><span>up to {{ format(model.yMax) }}</span></figcaption>
      <ul class="ts-legend">
        <li v-for="(s, i) in model.stats" :key="s.name" :class="tone(i)"><span class="swatch" aria-hidden="true" />{{ s.name }}<template v-if="s.count"> <strong>{{ format(s.latest) }}</strong> <small class="muted">highest {{ format(s.max) }}</small></template><small v-else class="muted"> no data</small></li>
        <li v-if="limit && model.limitY !== null" class="limit-key"><span class="swatch" aria-hidden="true" />{{ limit.label || 'limit' }} <strong>{{ format(limit.value) }}</strong></li>
      </ul>
      <ul v-if="model.marks.length" class="ts-marks" aria-label="Marked moments">
        <li v-for="(m, i) in model.marks" :key="i" :class="m.kind"><time>{{ clock(m.t) }}</time> {{ m.label }}</li>
      </ul>
    </template>
  </figure>
</template>
