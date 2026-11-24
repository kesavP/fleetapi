<script setup>
import { computed, inject, onMounted, ref } from 'vue'
import { formatBytes } from '../format'
import { HIDDEN, KIND_LABEL, diskLevel, formatRatio, formatSince, formatTtl, loadClickHouse, usedPercent } from '../api/clickhouse'

// How the telemetry service is connected to ClickHouse and what is stored there, as the project's read key sees it. Read-only: the
// connection is set by whoever deploys the service (chart values), and a browser holding a read key must not be able to change it.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')

const connected = computed(() => session.get())
const info = ref(null)
const loading = ref(false)
const error = ref('')
const checkedAt = ref(0)

async function load() {
  if (!connected.value) return
  loading.value = true
  error.value = ''
  try {
    info.value = await loadClickHouse(client)
    checkedAt.value = Date.now()
  } catch (e) {
    info.value = null
    error.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(load)

const conn = computed(() => (info.value ? info.value.connection : null))
const protocolOf = (p) => (p === 'http' ? 'http' : p === 'coroot' ? 'coroot' : 'native')
const shared = computed(() => !!(info.value && info.value.storage.some((s) => s.table_bytes != null)))
const rows = (n) => Number(n).toLocaleString('en-US')
const bytes = (n) => (n == null ? '–' : formatBytes(n))
</script>

<template>
  <section class="ch" aria-label="ClickHouse">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>

    <template v-else>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <p v-else-if="loading && !info" class="help" role="status">Asking the telemetry service…</p>

      <template v-if="info">
        <div class="panel">
          <fieldset class="protocol" disabled>
            <legend>Protocol</legend>
            <label v-for="p in ['native', 'http', 'coroot']" :key="p"><input type="radio" name="protocol" :value="p" :checked="protocolOf(conn.protocol) === p" /> {{ p === 'native' ? 'Native' : p === 'http' ? 'HTTP' : 'Coroot' }}</label>
          </fieldset>

          <div class="fields">
            <label>Address<input type="text" :value="HIDDEN" readonly aria-readonly="true" data-testid="address" /></label>
            <label>Credentials<input type="text" :value="HIDDEN" readonly aria-readonly="true" data-testid="user" /></label>
            <label>Password<input type="password" value="" placeholder="••••••••" readonly aria-readonly="true" data-testid="password" /></label>
            <label>Database<input type="text" :value="conn.database || ''" readonly aria-readonly="true" data-testid="database" /></label>
          </div>
          <div class="checks">
            <label><input type="checkbox" :checked="!!conn.tls" disabled /> Enable TLS</label>
            <label><input type="checkbox" disabled /> Skip TLS verify</label>
          </div>
          <p class="help">The address and credentials are set when the telemetry service is deployed (chart values) and are never sent to this page.</p>

          <div class="status" role="status">
            <span v-if="conn.reachable" class="ok">Connected<template v-if="conn.version"> · ClickHouse {{ conn.version }}</template><template v-if="conn.ping_ms != null"> · {{ Math.round(conn.ping_ms) }} ms</template></span>
            <span v-else class="bad">{{ conn.error || 'ClickHouse did not answer.' }}</span>
            <button type="button" class="secondary" :disabled="loading" @click="load">{{ loading ? 'Testing…' : 'Test connection' }}</button>
          </div>
        </div>

        <p v-for="w in info.warnings" :key="w" class="warn" role="note">{{ w }}</p>

        <h2>Storage Usage</h2>
        <p class="help">What this project has stored. Sizes are estimated from the project's share of each table's rows.</p>
        <table class="services ch-table" aria-label="Storage usage">
          <thead>
            <tr><th>Data type</th><th>Size</th><th>Rows</th><th>Compression</th><th>TTL</th><th>Data since</th><template v-if="shared"><th>Platform size</th><th>Platform rows</th></template></tr>
          </thead>
          <tbody>
            <tr v-for="s in info.storage" :key="s.type">
              <td>{{ KIND_LABEL[s.type] || s.type }}</td>
              <td>{{ bytes(s.estimated_bytes) }}</td>
              <td>{{ rows(s.rows) }}</td>
              <td>{{ formatRatio(s.compression) }}</td>
              <td>{{ formatTtl(s.ttl_seconds) }}</td>
              <td>{{ s.oldest ? formatSince(s.oldest) : '–' }}</td>
              <template v-if="shared"><td>{{ bytes(s.table_bytes) }}</td><td>{{ s.table_rows == null ? '–' : rows(s.table_rows) }}</td></template>
            </tr>
          </tbody>
        </table>

        <h2>Cluster Topology</h2>
        <p v-if="!info.cluster" class="help" data-testid="not-shared">The operator has not shared the server's disks. They appear here when the telemetry service runs with <code>exposeClusterInfo</code> on.</p>
        <table v-else class="services ch-table" aria-label="Cluster topology">
          <thead><tr><th>Server</th><th>Disk path</th><th>Free space</th><th>Total space</th><th>Usage</th></tr></thead>
          <tbody>
            <template v-for="srv in info.cluster.servers" :key="srv.host">
              <tr v-for="d in srv.disks" :key="srv.host + d.name">
                <td>{{ srv.host }}</td>
                <td>{{ d.path || d.name }}<br /><small class="muted">type: {{ d.unlimited ? 'Object storage' : d.type || 'Local' }}, name: {{ d.name }}</small></td>
                <td>{{ d.unlimited ? 'unlimited' : bytes(d.free) }}</td>
                <td>{{ d.unlimited ? 'unlimited' : bytes(d.total) }}</td>
                <td>
                  <template v-if="d.unlimited">{{ bytes(d.used) }} stored</template>
                  <template v-else-if="usedPercent(d) != null">
                    <span class="meter" :class="diskLevel(usedPercent(d))" role="meter" aria-label="Disk usage" aria-valuemin="0" aria-valuemax="100" :aria-valuenow="usedPercent(d)"><span :style="{ width: usedPercent(d) + '%' }"></span></span>
                    {{ usedPercent(d) }}%
                  </template>
                  <template v-else>–</template>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </template>
    </template>
  </section>
</template>
