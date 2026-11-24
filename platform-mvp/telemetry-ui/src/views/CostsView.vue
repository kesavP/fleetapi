<script setup>
import { computed, inject, onMounted, reactive, ref, watch } from 'vue'
import NotificationBell from '../components/NotificationBell.vue'
import { categoryOf } from '../api/applications'
import { COST_WINDOWS, barPercent, groupByCategory, lifecycleText, loadCosts, memoryText, money, shareText, sourceText, vcpuText } from '../api/costs'
import { createCategoryStore } from '../categories'

// What the infrastructure costs and where the money is wasted: the nodes (their price, what they used and what was requested of them, and the idle cost),
// the applications by category (what they used, were allocated and over-provisioned), and an analysis of the idle cost. Every figure is a month's worth
// at the rate of the window picked, at list prices. A FEATURE the hub switches on: while it is off the hub says so and this page reads nothing.
const props = defineProps({
  client: { type: Object, default: null },
  session: { type: Object, default: null },
  features: { type: Object, default: null },
  categories: { type: Object, default: null },
  loaders: { type: Object, default: null },
})
const client = props.client || inject('client')
const session = props.session || inject('session')
const features = props.features || inject('features', null)
const store = props.categories || inject('categories', null) || createCategoryStore({ session, storage: null })
const load = (props.loaders && props.loaders.loadCosts) || loadCosts

const connected = computed(() => session.get())
// off: the hub has not switched the costs on; checking: it has not said yet; on: read them. Without a feature store there is nothing to wait for.
const gate = computed(() => (!features ? 'on' : !features.state.loaded ? 'checking' : features.state.costs ? 'on' : 'off'))

const seconds = ref(COST_WINDOWS[0].seconds)
const report = ref(null)
const loading = ref(false)
const error = ref('')

async function refresh() {
  if (!connected.value || gate.value !== 'on') return
  loading.value = true
  error.value = ''
  try {
    report.value = await load(client, seconds.value)
  } catch (e) {
    report.value = null
    error.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(refresh)
watch(gate, refresh)
watch(seconds, refresh)

const totals = computed(() => (report.value ? report.value.totals : null))
const hasRequests = computed(() => !!report.value && report.value.hasRequests)
const price = computed(() => (totals.value ? totals.value.priceMonthly : 0))
const notifications = computed(() => (report.value ? report.value.warnings : []))

const pct = (v) => (price.value > 0 ? (100 * v) / price.value : 0)
const segments = computed(() =>
  totals.value
    ? [
        { key: 'used', label: 'Used', value: totals.value.usedMonthly },
        { key: 'overprovisioned', label: 'Requested, not used', value: totals.value.overprovisionedMonthly },
        { key: 'idle', label: 'Idle', value: totals.value.idleMonthly },
      ]
    : [],
)

const groups = computed(() => (report.value ? groupByCategory(report.value.applications, (a) => store.categoryFor(a.app, categoryOf(a.namespace))) : []))
const appTotals = computed(() => (totals.value ? { usage: totals.value.appUsageMonthly, allocation: totals.value.appAllocationMonthly, over: totals.value.appOverprovisioningMonthly } : null))
const open = reactive({})
const toggle = (c) => {
  open[c] = !open[c]
}

const nodeSubtitle = (n) => [n.instanceType, vcpuText(n.cpu.capacity), memoryText(n.memory.capacity)].filter(Boolean).join(' / ')
const KIND = { saving: 'Saving', info: 'Info', warning: 'Warning' }
</script>

<template>
  <div class="page-head">
    <h1>Costs</h1>
    <div class="head-right">
      <label for="window" class="sr-only">Time range</label>
      <select id="window" v-model.number="seconds">
        <option v-for="w in COST_WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option>
      </select>
      <NotificationBell :items="notifications" />
      <button type="button" class="secondary" :disabled="loading || gate !== 'on'" @click="refresh">{{ loading ? 'Loading…' : 'Refresh' }}</button>
    </div>
  </div>

  <section v-if="!connected" class="panel"><p class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p></section>
  <section v-else-if="gate === 'off'" class="panel" data-testid="costs-off">
    <h2>The costs are switched off</h2>
    <p class="help">
      This hub has not switched the infrastructure costs on, so there is nothing to show. To switch them on, set <code>telemetryIngest.costs.enabled</code> to <code>true</code> in the hub chart's values and apply
      it. Nodes are then priced from your own prices (<code>telemetryIngest.costs.prices</code>) and, for Azure, from the public price list.
    </p>
  </section>
  <section v-else-if="gate === 'checking'" class="panel"><p class="help" role="status">Checking what this hub offers…</p></section>
  <section v-else-if="error" class="panel"><p class="error" role="alert" data-testid="costs-error">{{ error }}</p></section>
  <section v-else-if="loading && !report" class="panel"><p class="help" role="status">Working out the costs…</p></section>
  <template v-else-if="report">
    <section v-if="!report.nodes.length" class="panel">
      <p class="help" data-testid="no-nodes">No node sent its size and use in this window, so there is nothing to cost. Are the node-agents running, and sending to this project?</p>
    </section>
    <template v-else>
      <section class="panel" data-testid="summary">
        <div class="tiles">
          <div class="tile" data-testid="tile-price"><span class="k">What the nodes cost</span><strong>{{ money(totals.priceMonthly) }}<small>/mo</small></strong><span class="sub">{{ report.nodes.length }} node{{ report.nodes.length === 1 ? '' : 's' }}</span></div>
          <div class="tile" data-testid="tile-used"><span class="k">Used</span><strong>{{ money(totals.usedMonthly) }}<small>/mo</small></strong><span class="sub">{{ shareText(totals.usedMonthly, price) }} of the cost</span></div>
          <div class="tile" data-testid="tile-overprovisioned">
            <span class="k">Requested, not used</span>
            <template v-if="hasRequests"><strong>{{ money(totals.overprovisionedMonthly) }}<small>/mo</small></strong><span class="sub">{{ shareText(totals.overprovisionedMonthly, price) }} of the cost</span></template>
            <template v-else><strong>—</strong><span class="sub">needs the cluster collector</span></template>
          </div>
          <div class="tile idle" data-testid="tile-idle">
            <span class="k">Idle</span><strong>{{ money(totals.idleMonthly) }}<small>/mo</small></strong>
            <span class="sub">{{ shareText(totals.idleMonthly, price) }} of the cost, {{ hasRequests ? 'requested by nobody' : 'not used' }}</span>
          </div>
        </div>
        <div v-if="price > 0" class="split" data-testid="split" role="img" :aria-label="`Of ${money(price)} a month: ${segments.map((s) => `${s.label} ${money(s.value)}`).join(', ')}`">
          <span v-for="s in segments" :key="s.key" class="seg" :class="s.key" :style="{ width: pct(s.value) + '%' }" :title="`${s.label}: ${money(s.value)}`" />
        </div>
        <p v-if="totals.nodesWithoutPrice" class="help">{{ totals.nodesWithoutPrice }} node{{ totals.nodesWithoutPrice === 1 ? ' has' : 's have' }} no price and {{ totals.nodesWithoutPrice === 1 ? 'is' : 'are' }} left out of the money.</p>
      </section>

      <section class="panel flush">
        <h2 class="pad">Nodes</h2>
        <div class="scroll">
          <table class="services costs-table" data-testid="nodes" aria-label="Nodes">
            <thead><tr><th>Node</th><th>CPU</th><th>Memory</th><th class="num">Price</th><th class="num">Idle cost</th></tr></thead>
            <tbody>
              <tr v-for="n in report.nodes" :key="n.name" :data-node="n.name">
                <td><span class="name">{{ n.name }}</span><br /><small class="muted">{{ nodeSubtitle(n) }}</small></td>
                <td>
                  <div class="bar-row" data-testid="cpu-usage"><span class="lbl">usage</span><span class="bar"><span class="fill usage" :style="{ width: barPercent(n.cpu.usage, n.cpu.capacity) + '%' }" /></span><span class="val">{{ n.cpu.usage.toFixed(2).replace(/\.?0+$/, '') }}</span></div>
                  <div class="bar-row" data-testid="cpu-requests">
                    <span class="lbl">request</span>
                    <template v-if="n.cpu.requests !== null"><span class="bar"><span class="fill requests" :style="{ width: barPercent(n.cpu.requests, n.cpu.capacity) + '%' }" /></span><span class="val">{{ n.cpu.requests.toFixed(2).replace(/\.?0+$/, '') }}</span></template>
                    <span v-else class="muted">unknown</span>
                  </div>
                </td>
                <td>
                  <div class="bar-row" data-testid="mem-usage"><span class="lbl">usage</span><span class="bar"><span class="fill usage" :style="{ width: barPercent(n.memory.usage, n.memory.capacity) + '%' }" /></span><span class="val">{{ memoryText(n.memory.usage) }}</span></div>
                  <div class="bar-row" data-testid="mem-requests">
                    <span class="lbl">request</span>
                    <template v-if="n.memory.requests !== null"><span class="bar"><span class="fill requests" :style="{ width: barPercent(n.memory.requests, n.memory.capacity) + '%' }" /></span><span class="val">{{ memoryText(n.memory.requests) }}</span></template>
                    <span v-else class="muted">unknown</span>
                  </div>
                </td>
                <td class="num" data-testid="price">
                  <template v-if="n.priceMonthly !== null"><strong>{{ money(n.priceMonthly) }}</strong><small class="muted">/mo</small></template>
                  <span v-else class="muted">no price</span>
                  <br /><small class="muted">{{ [lifecycleText(n), sourceText(n.priceSource)].filter(Boolean).join(', ') }}</small>
                </td>
                <td class="num" data-testid="idle">{{ n.priceMonthly !== null ? money(n.idleMonthly) : '—' }}</td>
              </tr>
            </tbody>
            <tfoot>
              <tr data-testid="nodes-total"><td><strong>TOTAL</strong></td><td /><td /><td class="num"><strong>{{ money(totals.priceMonthly) }}</strong><small class="muted">/mo</small></td><td class="num"><strong>{{ money(totals.idleMonthly) }}</strong><small class="muted">/mo</small></td></tr>
            </tfoot>
          </table>
        </div>
      </section>

      <section class="panel flush">
        <h2 class="pad">Applications</h2>
        <p v-if="!hasRequests" class="help pad" data-testid="requests-note">What pods request is not known, so allocation and over-provisioning are not shown. Deploy the cluster collector to see them.</p>
        <div class="scroll">
          <table class="services costs-table" data-testid="categories" aria-label="Applications by category">
            <thead><tr><th>Category</th><th class="num">Usage costs</th><th class="num">Allocation costs</th><th class="num">Over-provisioning costs</th></tr></thead>
            <tbody>
              <template v-for="g in groups" :key="g.category">
                <tr :data-category="g.category">
                  <td><button type="button" class="link" data-testid="cat-toggle" :aria-expanded="open[g.category] ? 'true' : 'false'" @click="toggle(g.category)">{{ open[g.category] ? '▾' : '▸' }} {{ g.category }}</button> <small class="muted">{{ g.apps.length }}</small></td>
                  <td class="num">{{ money(g.usage) }}</td>
                  <td class="num">{{ g.allocation === null ? '—' : money(g.allocation) }}</td>
                  <td class="num">{{ g.overprovisioning === null ? '—' : money(g.overprovisioning) }}</td>
                </tr>
                <template v-if="open[g.category]">
                  <tr v-for="a in g.apps" :key="a.app" class="sub-row" :data-app="a.app">
                    <td class="indent"><router-link :to="{ name: 'application', params: { name: a.app } }">{{ a.label }}</router-link></td>
                    <td class="num">{{ money(a.usageMonthly) }}</td>
                    <td class="num">{{ a.allocationMonthly === null ? '—' : money(a.allocationMonthly) }}</td>
                    <td class="num">{{ a.overprovisioningMonthly === null ? '—' : money(a.overprovisioningMonthly) }}</td>
                  </tr>
                </template>
              </template>
            </tbody>
            <tfoot>
              <tr v-if="totals.unattributedMonthly > 0" data-testid="unattributed"><td class="muted">System and other (not a Kubernetes workload)</td><td class="num">{{ money(totals.unattributedMonthly) }}</td><td class="num">—</td><td class="num">—</td></tr>
              <tr data-testid="categories-total">
                <td><strong>TOTAL</strong></td><td class="num"><strong>{{ money(appTotals.usage) }}</strong></td>
                <td class="num"><strong>{{ appTotals.allocation === null ? '—' : money(appTotals.allocation) }}</strong></td>
                <td class="num"><strong>{{ appTotals.over === null ? '—' : money(appTotals.over) }}</strong></td>
              </tr>
            </tfoot>
          </table>
        </div>
      </section>

      <section class="panel" data-testid="analysis">
        <h2>Idle cost analysis</h2>
        <p v-if="!report.analysis.length" class="help">Nothing to report: what the nodes cost is being used.</p>
        <article v-for="f in report.analysis" :key="f.id" class="finding" :class="f.kind">
          <header><span class="pill" :class="f.kind">{{ KIND[f.kind] }}</span><h3>{{ f.title }}</h3></header>
          <p>{{ f.detail }}</p>
          <p v-if="f.monthlySaving !== null" class="saving" data-testid="saving">Could save about <strong>{{ money(f.monthlySaving) }}</strong>/mo</p>
          <ul v-if="f.items.length"><li v-for="it in f.items" :key="it.name">{{ it.name }} <strong>{{ money(it.monthly) }}</strong></li></ul>
        </article>
      </section>

      <p class="help" data-testid="costs-notes">
        Figures are a month (730 hours) at the rate of the last {{ COST_WINDOWS.find((w) => w.seconds === seconds)?.label }}, at list prices: reservations and discounts make the real cost lower.
        A node's price is split between its CPU and memory by what each is worth. <strong>Used</strong> is what workloads used, <strong>requested, not used</strong> is what pods asked for and did not use, and
        <strong>idle</strong> is what {{ hasRequests ? 'no pod requested' : 'nothing used' }}.
      </p>
    </template>
  </template>
</template>
