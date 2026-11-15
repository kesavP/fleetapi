<script setup>
// The cards plugins made (see ../cards/registry.js), every one drawn the same way. A card is rows of text and nothing else, so it is
// shown as text: no markup of a plugin's own ever reaches the page.
defineProps({ cards: { type: Array, default: () => [] } })
</script>

<template>
  <div v-if="cards.length" class="cards" data-section="plugins">
    <div v-for="c in cards" :id="c.id" :key="c.id" class="card" :data-card="c.id">
      <h3>{{ c.title }}</h3>
      <dl>
        <div v-for="r in c.rows" :key="r.label"><dt>{{ r.label }}</dt><dd :class="r.level">{{ r.value }}</dd></div>
      </dl>
      <p v-if="c.note" class="help">{{ c.note }}</p>
    </div>
  </div>
</template>
