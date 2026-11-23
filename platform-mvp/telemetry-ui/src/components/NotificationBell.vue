<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import AppIcon from './AppIcon.vue'

// A bell for the page header: the notes a page has about what it could not read or had to guess, kept out of the way until someone wants them. The count
// on the bell says how many there are, and pressing it opens the list. `items` are strings, or {text, testid} where a test needs to find one; an item with no
// text is not one. The list closes on Escape and on a click anywhere else.
const props = defineProps({ items: { type: Array, default: () => [] } })

const notes = computed(() =>
  props.items
    .map((i) => (typeof i === 'string' ? { text: i } : i || {}))
    .filter((i) => typeof i.text === 'string' && i.text.trim() !== ''),
)
const count = computed(() => notes.value.length)
const label = computed(() => `Notifications: ${count.value || 'none'}`)
const shown = computed(() => (count.value > 99 ? '99+' : String(count.value)))

const open = ref(false)
const root = ref(null)
const button = ref(null)

function onDocumentClick(e) {
  if (open.value && root.value && !root.value.contains(e.target)) open.value = false
}
function onKey(e) {
  if (open.value && e.key === 'Escape') {
    open.value = false
    if (button.value) button.value.focus()
  }
}
onMounted(() => {
  document.addEventListener('click', onDocumentClick)
  document.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div ref="root" class="bell-wrap">
    <button ref="button" type="button" class="secondary bell" data-testid="bell" :aria-label="label" :title="label" aria-haspopup="true" :aria-expanded="open ? 'true' : 'false'" @click="open = !open">
      <AppIcon name="alerts" />
      <span v-if="count" class="bell-count" data-testid="bell-count" aria-hidden="true">{{ shown }}</span>
    </button>
    <div v-if="open" class="bell-pop" role="region" aria-label="Notifications" data-testid="bell-list">
      <ul v-if="count">
        <li v-for="(n, i) in notes" :key="i" :data-testid="n.testid || null">{{ n.text }}</li>
      </ul>
      <p v-else class="muted">Nothing to report.</p>
    </div>
  </div>
</template>
