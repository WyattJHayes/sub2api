import { defineStore } from 'pinia'
import { ref } from 'vue'
import radarAdminAPI, { type RadarAlert, type RadarDataset, type RadarGate, type RadarModelHealth, type RadarOverview, type RadarRun, type RadarWorker } from '@/api/admin/radar'

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
      error.value = err instanceof Error ? err.message : 'Unable to load radar data'
    } finally {
      loading.value = false
    }
  }

  async function loadSection(section: 'models' | 'runs' | 'alerts' | 'gates' | 'workers' | 'datasets'): Promise<void> {
    try {
      if (section === 'models') models.value = await radarAdminAPI.models()
      if (section === 'runs') runs.value = await radarAdminAPI.runs()
      if (section === 'alerts') alerts.value = await radarAdminAPI.alerts()
      if (section === 'gates') gates.value = await radarAdminAPI.gates()
      if (section === 'workers') workers.value = await radarAdminAPI.workers()
      if (section === 'datasets') datasets.value = await radarAdminAPI.datasets()
    } catch (err) {
      error.value = err instanceof Error ? err.message : 'Unable to load radar section'
    }
  }

  return { overview, models, runs, alerts, gates, workers, datasets, loading, error, refresh, loadSection }
})
