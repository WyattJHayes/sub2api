import { defineStore } from 'pinia'
import { ref } from 'vue'
import radarAdminAPI, { type RadarAlert, type RadarDataset, type RadarGate, type RadarModelHealth, type RadarOverview, type RadarRun, type RadarWorker } from '@/api/admin/radar'
import { i18n } from '@/i18n'

export type RadarSection = 'overview' | 'models' | 'runs' | 'datasets'
export type RadarDataSection = RadarSection | 'alerts' | 'gates' | 'workers'

export const useRadarStore = defineStore('radar', () => {
  const overview = ref<RadarOverview | null>(null)
  const models = ref<RadarModelHealth[]>([])
  const alerts = ref<RadarAlert[]>([])
  const gates = ref<RadarGate[]>([])
  const workers = ref<RadarWorker[]>([])
  const datasets = ref<RadarDataset[]>([])
  const runs = ref<RadarRun[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function refresh(): Promise<void> {
    loading.value = true
    error.value = null
    try {
      const data = await radarAdminAPI.overview()
      overview.value = data
      models.value = data.models ?? []
      alerts.value = data.alerts ?? []
      gates.value = data.gates ?? []
      workers.value = data.workers ?? []
    } catch (err) {
      error.value = err instanceof Error ? err.message : i18n.global.t('admin.radar.messages.loadFailed')
    } finally {
      loading.value = false
    }
  }

  async function loadSection(section: Exclude<RadarDataSection, 'overview'>): Promise<void> {
    try {
      if (section === 'models') models.value = await radarAdminAPI.models()
      if (section === 'runs') runs.value = await radarAdminAPI.runs()
      if (section === 'alerts') alerts.value = await radarAdminAPI.alerts()
      if (section === 'gates') gates.value = await radarAdminAPI.gates()
      if (section === 'workers') workers.value = await radarAdminAPI.workers()
      if (section === 'datasets') datasets.value = await radarAdminAPI.datasets()
    } catch (err) {
      error.value = err instanceof Error ? err.message : i18n.global.t('admin.radar.messages.sectionLoadFailed')
    }
  }

  return { overview, models, runs, alerts, gates, workers, datasets, loading, error, refresh, loadSection }
})
