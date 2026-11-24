<script setup>
import { nextTick, ref } from 'vue'

// The checkboxes that choose which categories of application are shown, with a "+" to make a category of one's own.
//   categories: [name]   every category
//   modelValue: [name]   the ones shown (at least one stays on: none would show nothing)
//   validate(name)       why a new name cannot be used, or ''
const props = defineProps({
  categories: { type: Array, default: () => [] },
  modelValue: { type: Array, default: () => [] },
  validate: { type: Function, default: () => '' },
})
const emit = defineEmits(['update:modelValue', 'add'])

function toggle(key, event) {
  const on = props.modelValue.includes(key)
  const next = on ? props.modelValue.filter((c) => c !== key) : [...props.modelValue, key]
  if (next.length) emit('update:modelValue', next)
  // The browser has already flipped the box. When the change was refused nothing re-renders, so put the box back.
  event.target.checked = next.length ? !on : on
}

const adding = ref(false)
const name = ref('')
const problem = ref('')
const input = ref(null)
async function open() {
  adding.value = !adding.value
  name.value = ''
  problem.value = ''
  if (adding.value) {
    await nextTick()
    if (input.value) input.value.focus()
  }
}
function submit() {
  problem.value = props.validate(name.value)
  if (problem.value) return
  emit('add', name.value)
  adding.value = false
}
</script>

<template>
  <div class="cats" role="group" aria-label="Kinds of namespace">
    <label v-for="c in categories" :key="c" class="cat"><input type="checkbox" :checked="modelValue.includes(c)" @change="toggle(c, $event)" /> {{ c }}</label>
    <span class="cat-add">
      <button type="button" class="cat-plus" aria-label="Add a category" :aria-expanded="adding" title="Add a category" @click="open">+</button>
      <form v-if="adding" class="cat-form" @submit.prevent="submit">
        <input ref="input" v-model="name" type="text" aria-label="Name of the new category" placeholder="new category" autocomplete="off" :aria-invalid="!!problem" @keydown.esc="adding = false" />
        <button type="submit">Add</button>
        <p v-if="problem" class="error" role="alert">{{ problem }}</p>
      </form>
    </span>
  </div>
</template>
