<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

// A drop-down for choosing namespaces: any number, or none, which means all of them. The chosen ones show as chips in the box,
// each with a way to remove it; the list has a filter, select-all and clear.
//   namespaces: [{name, count}]   what can be chosen
//   modelValue: [name]            what is chosen (empty: all)
const props = defineProps({
  namespaces: { type: Array, default: () => [] },
  modelValue: { type: Array, default: () => [] },
})
const emit = defineEmits(['update:modelValue'])

const open = ref(false)
const filter = ref('')
const root = ref(null)
const search = ref(null)
const MAX_CHIPS = 2

// A choice that is no longer on offer (its namespace is gone, or hidden by a category) is not shown as chosen.
const offered = computed(() => new Set(props.namespaces.map((n) => n.name)))
const chosen = computed(() => props.modelValue.filter((n) => offered.value.has(n)))
const shown = computed(() => {
  const f = filter.value.trim().toLowerCase()
  return f ? props.namespaces.filter((n) => n.name.toLowerCase().includes(f)) : props.namespaces
})
const allCount = computed(() => props.namespaces.reduce((a, n) => a + n.count, 0))

const set = (names) => emit('update:modelValue', names)
function toggle(name) {
  set(chosen.value.includes(name) ? chosen.value.filter((n) => n !== name) : [...chosen.value, name])
}
const remove = (name) => set(chosen.value.filter((n) => n !== name))
const selectShown = () => set([...new Set([...chosen.value, ...shown.value.map((n) => n.name)])])
const clear = () => set([])

async function toggleOpen() {
  open.value = !open.value
  if (open.value) {
    await nextTick()
    if (search.value) search.value.focus()
  }
}
const close = () => (open.value = false)
function outside(e) {
  if (open.value && root.value && !root.value.contains(e.target)) close()
}
onMounted(() => document.addEventListener('mousedown', outside))
onBeforeUnmount(() => document.removeEventListener('mousedown', outside))
</script>

<template>
  <div ref="root" class="ns-picker" @keydown.esc="close">
    <span class="ns-label" id="ns-label">namespaces</span>
    <div class="ns-box" :class="{ open }">
      <div class="ns-chips">
        <span v-if="!chosen.length" class="ns-all">all ({{ allCount }})</span>
        <span v-for="n in chosen.slice(0, MAX_CHIPS)" :key="n" class="ns-chip">
          {{ n }}<button type="button" class="chip-x" :aria-label="`Remove namespace ${n}`" @click="remove(n)">×</button>
        </span>
        <span v-if="chosen.length > MAX_CHIPS" class="ns-more">+{{ chosen.length - MAX_CHIPS }} more</span>
      </div>
      <button type="button" class="ns-toggle" aria-labelledby="ns-label" aria-haspopup="true" :aria-expanded="open" @click="toggleOpen">
        <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true"><path d="M7 10l5 5 5-5z" fill="currentColor" /></svg>
      </button>
    </div>

    <div v-if="open" class="ns-pop" role="group" aria-label="Choose namespaces">
      <input ref="search" v-model="filter" class="ns-search" type="search" placeholder="filter namespaces" autocomplete="off" aria-label="Filter namespaces" />
      <div class="ns-actions">
        <button type="button" class="link" @click="selectShown">Select {{ filter.trim() ? 'matching' : 'all' }}</button>
        <button type="button" class="link" :disabled="!chosen.length" @click="clear">Clear</button>
      </div>
      <ul class="ns-list">
        <li v-for="n in shown" :key="n.name">
          <label class="ns-option"><input type="checkbox" :checked="chosen.includes(n.name)" @change="toggle(n.name)" /> <span class="ns-name">{{ n.name }}</span> <span class="muted">{{ n.count }}</span></label>
        </li>
        <li v-if="!shown.length" class="muted ns-none">{{ namespaces.length ? 'No namespace matches.' : 'No namespaces.' }}</li>
      </ul>
    </div>
  </div>
</template>
