<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { CARD_W, fitScale, linkPath, placeCards } from '../incidents/graphLayout'
import { issueLabel } from '../incidents/narrative'

// The applications an incident touched and the calls between them, drawn the way the service map is: a card each, a line per call, red
// where the call has an issue. The hurt application is on the left, what it depends on to the right; the cause is outlined.
const props = defineProps({
  nodes: { type: Array, required: true }, // [{id, name, depth, issues}]
  edges: { type: Array, required: true }, // [{from, to, issues}]
  app: { type: String, required: true }, // the hurt application
  cause: { type: Object, default: null }, // findings.rootCause
})

const placed = computed(() => placeCards(props.nodes, props.edges))

// A wide chain of dependencies is shrunk to the width of the page instead of being scrolled, down to a size that can still be read.
const box = ref(null)
const available = ref(0)
const scale = computed(() => fitScale(available.value, placed.value.width))
let watcher = null
onMounted(() => {
  if (!box.value) return
  available.value = box.value.clientWidth
  if (typeof ResizeObserver !== 'undefined') {
    watcher = new ResizeObserver(() => (available.value = box.value ? box.value.clientWidth : 0))
    watcher.observe(box.value)
  }
})
onBeforeUnmount(() => watcher && watcher.disconnect())
const lines = computed(() =>
  props.edges
    .map((e) => ({ key: `${e.from}>${e.to}`, bad: e.issues.length > 0, d: placed.value.cards.has(e.from) && placed.value.cards.has(e.to) ? linkPath(placed.value.cards.get(e.from), placed.value.cards.get(e.to)) : '' }))
    .filter((l) => l.d),
)
const causeIds = computed(() => (props.cause ? new Set(props.cause.scope === 'edge' ? [props.cause.from, props.cause.to] : [props.cause.app]) : new Set()))
const style = (id) => {
  const c = placed.value.cards.get(id)
  return { left: `${c.x}px`, top: `${c.y}px`, width: `${CARD_W}px`, minHeight: `${c.h}px` }
}
</script>

<template>
  <div ref="box" class="ig-scroll">
    <div class="ig-fit" :style="{ width: placed.width * scale + 'px', height: placed.height * scale + 'px' }">
    <div class="ig" :style="{ width: placed.width + 'px', height: placed.height + 'px', transform: `scale(${scale})` }">
      <svg class="ig-lines" :width="placed.width" :height="placed.height" aria-hidden="true">
        <defs>
          <marker id="ig-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" fill="currentColor" /></marker>
        </defs>
        <path v-for="l in lines" :key="l.key" :d="l.d" class="ig-line" :class="{ bad: l.bad }" marker-end="url(#ig-arrow)" />
      </svg>
      <router-link
        v-for="n in nodes"
        :key="n.id"
        :to="{ name: 'application', params: { name: n.id } }"
        class="ig-card"
        :class="{ hurt: n.id === app, culprit: causeIds.has(n.id) }"
        :style="style(n.id)"
      >
        <strong>{{ n.name }}</strong>
        <template v-if="n.issues.length">
          <small>Issues:</small>
          <ul>
            <li v-for="(i, k) in n.issues" :key="k">{{ issueLabel(i) }}</li>
          </ul>
        </template>
      </router-link>
    </div>
    </div>
  </div>
</template>
