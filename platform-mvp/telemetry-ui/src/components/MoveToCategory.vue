<script setup>
import { computed, nextTick, ref } from 'vue'

// "Move the app to a category": a menu listing the other categories and a way to make a new one.
//   current: the category the application is in now
//   categories: every category
//   validate(name): why a new name cannot be used, or ''
const props = defineProps({
  current: { type: String, default: '' },
  categories: { type: Array, default: () => [] },
  validate: { type: Function, default: () => '' },
})
const emit = defineEmits(['move', 'add', 'close'])

const others = computed(() => props.categories.filter((c) => c !== props.current))
const adding = ref(false)
const name = ref('')
const problem = ref('')
const input = ref(null)
async function startAdding() {
  adding.value = true
  await nextTick()
  if (input.value) input.value.focus()
}
function submit() {
  problem.value = props.validate(name.value)
  if (problem.value) return
  emit('add', name.value) // makes it and moves the application to it
}
</script>

<template>
  <div class="move-menu" role="menu" aria-label="Move the app to a category" @keydown.esc.stop="emit('close')" @click.stop>
    <p class="move-title">Move the app to a category</p>
    <button type="button" class="move-item" role="menuitem" @click="startAdding"><span aria-hidden="true">+</span> a new category</button>
    <form v-if="adding" class="cat-form" @submit.prevent="submit">
      <input ref="input" v-model="name" type="text" aria-label="Name of the new category" placeholder="new category" autocomplete="off" :aria-invalid="!!problem" />
      <button type="submit">Add</button>
      <p v-if="problem" class="error" role="alert">{{ problem }}</p>
    </form>
    <button v-for="c in others" :key="c" type="button" class="move-item" role="menuitem" @click="emit('move', c)"><span aria-hidden="true">→</span> {{ c }}</button>
  </div>
</template>
