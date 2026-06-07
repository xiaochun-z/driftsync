<script setup lang="ts">
import { ref, onMounted, nextTick, watch } from 'vue'
import { EventsOn, BrowserOpenURL } from '../wailsjs/runtime/runtime'
import { GetConfig, SaveConfig, StartSync, StopSync, SelectDirectory, OpenDirectory, LocateFile, GetRemoteItems, StartOver, GetStartOverPaths, SetWorkspaceLocation } from '../wailsjs/go/main/App'
import FolderNode from './FolderNode.vue'

const currentTab = ref('dashboard')
const isSyncing = ref(false)
const isSetupComplete = ref(localStorage.getItem('driftsync_setup_done') === 'true')

const config = ref<any>({
  Tenant: '',
  ClientID: '',
  LocalPath: '',
  DownloadFromCloud: true,
  UploadFromLocal: true,
  DownloadWorkers: 8,
  UploadWorkers: 8,
  UploadChunkMB: 8,
  UploadParallel: 2,
})

const deviceCode = ref('')
const verificationURI = ref('')
const showAuthModal = ref(false)
const syncLog = ref<string[]>([])
const isCopied = ref(false)

const logContainer = ref<HTMLElement | null>(null)
const isScrolledUp = ref(false)

// Settings Accordions State
const openAccordion = ref<string | null>(null)
const toggleAccordion = (name: string) => {
  openAccordion.value = openAccordion.value === name ? null : name
}

// Start Over State
const showStartOverModal = ref(false)
const agreeToStartOver = ref(false)
const startOverLoading = ref(false)
const isSaveSuccess = ref(false)
const isSaving = ref(false)
const pathsToDelete = ref<string[]>([])

const openStartOverModal = async () => {
  agreeToStartOver.value = false
  pathsToDelete.value = await GetStartOverPaths()
  showStartOverModal.value = true
}

const handleStartOver = async () => {
  startOverLoading.value = true
  try {
    await StartOver()
    localStorage.removeItem('driftsync_setup_done')
    window.location.reload()
  } catch (e: any) {
    alert("Failed to start over: " + e)
  } finally {
    startOverLoading.value = false
  }
}

const wizardRootNodes = ref<any[]>([])
const loadingWizardNodes = ref(false)
const wizardSelectAll = ref(false)
const folderNodeRefs = ref<any[]>([])
const setupFolderNodeRefs = ref<any[]>([])

const loadWizardNodes = async () => {
  loadingWizardNodes.value = true
  try {
    const items = await GetRemoteItems("")
    wizardRootNodes.value = items.map((item: any) => ({
      name: item.name,
      path: "/" + item.name,
      isFolder: item.folder !== undefined,
      children: []
    }))
  } catch (e) {
    console.error("Failed to load root folders:", e)
  }
  loadingWizardNodes.value = false
}

const handleLogScroll = () => {
  if (!logContainer.value) return
  const { scrollTop, scrollHeight, clientHeight } = logContainer.value
  isScrolledUp.value = scrollHeight - scrollTop - clientHeight > 10
}

watch(syncLog, () => {
  if (!isScrolledUp.value) {
    nextTick(() => {
      if (logContainer.value) {
        logContainer.value.scrollTop = logContainer.value.scrollHeight
      }
    })
  }
}, { deep: true })

watch(currentTab, (newTab) => {
  if (newTab === 'settings' && wizardRootNodes.value.length === 0 && !loadingWizardNodes.value) {
    loadWizardNodes()
  }
})

const loadConfig = async () => {
  try {
    const c = await GetConfig()
    config.value = c

    // If backend indicates it's the first run, ensure UI also goes through setup
    if (c.is_first_run) {
      localStorage.removeItem('driftsync_setup_done')
      isSetupComplete.value = false
    }
  } catch (e) {
    console.error("Failed to load config:", e)
  }
}

onMounted(async () => {
  EventsOn("deviceCode", (data: any) => {
    deviceCode.value = data.userCode
    verificationURI.value = data.verificationURI
    showAuthModal.value = true
  })

  EventsOn("loginSuccess", () => {
    showAuthModal.value = false
    syncLog.value.push("Login successful.")
  })

  await loadConfig()

  EventsOn("syncEvent", (msg: string) => {
    syncLog.value.push("> " + msg)
  })

  EventsOn("syncStarted", () => {
    isSyncing.value = true
    syncLog.value.push("Sync started...")
  })

  EventsOn("syncCompleted", () => {
    isSyncing.value = false
    syncLog.value.push("Sync completed.")
    // Invalidate the frontend tree cache to reflect deleted/added files
    wizardRootNodes.value = []
    if (currentTab.value === 'settings' || currentTab.value === 'setup') {
      loadWizardNodes()
    }
  })
})

const saveConfig = async () => {
  if (isSaving.value) return;
  isSaving.value = true;
  try {
    const rules: string[] = []
    if (folderNodeRefs.value) {
      folderNodeRefs.value.forEach((n: any) => {
        if (n && typeof n.getExcludeRules === 'function') {
          rules.push(...n.getExcludeRules())
        }
      })
    }
    
    if (!config.value.Sync) {
      config.value.Sync = {}
    }
    config.value.Sync.Exclude = rules

    await SaveConfig(config.value)
    
    isSaveSuccess.value = true
    setTimeout(() => {
      isSaveSuccess.value = false
      isSaving.value = false
      currentTab.value = 'dashboard'
    }, 800)
  } catch (e: any) {
    alert("Failed to save: " + e)
    isSaving.value = false
  }
}

const completeSetup = async () => {
  try {
    const rules: string[] = []
    if (setupFolderNodeRefs.value) {
      setupFolderNodeRefs.value.forEach((n: any) => {
        if (n && typeof n.getExcludeRules === 'function') {
          rules.push(...n.getExcludeRules())
        }
      })
    }
    
    if (!config.value.Sync) {
      config.value.Sync = {}
    }
    config.value.Sync.Exclude = rules

    await SaveConfig(config.value)
    localStorage.setItem('driftsync_setup_done', 'true')
    isSetupComplete.value = true
    
    currentTab.value = 'dashboard'
    // Start actual sync automatically
    await startSync()
  } catch (e: any) {
    alert("Failed to complete setup: " + e)
  }
}

const selectDir = async () => {
  const dir = await SelectDirectory()
  if (dir) {
    config.value.LocalPath = dir
  }
}

const changeWorkspaceDir = async () => {
  const dir = await SelectDirectory()
  if (dir) {
    try {
      await SetWorkspaceLocation(dir)
      alert("Workspace location saved! Please restart the application for the changes to take effect.")
    } catch (e) {
      alert("Failed to set workspace location: " + e)
    }
  }
}

const openDir = async () => {
  if (config.value.LocalPath) {
    try {
      await OpenDirectory(config.value.LocalPath)
    } catch (err) {
      console.error("Failed to open directory:", err)
    }
  }
}

const locateConfig = async () => {
  try {
    await LocateFile("config.yaml")
  } catch (err) {
    console.error("Failed to locate config.yaml:", err)
  }
}

const locateDb = async () => {
  try {
    await LocateFile("driftsync.db")
  } catch (err) {
    console.error("Failed to locate driftsync.db:", err)
  }
}

const startSync = async () => {
  if (localStorage.getItem('driftsync_setup_done') !== 'true') {
    syncLog.value = ["Authenticating and fetching remote directories..."]
    isSyncing.value = true
    try {
      await loadWizardNodes()
      currentTab.value = 'setup'
      syncLog.value = []
    } catch (e: any) {
      syncLog.value.push("Error: " + e)
    } finally {
      isSyncing.value = false
    }
    return
  }

  syncLog.value = []
  isSyncing.value = true
  try {
    await StartSync()
  } catch (e: any) {
    syncLog.value.push("Error: " + e)
    isSyncing.value = false
  }
}

const stopSync = async () => {
  await StopSync()
  isSyncing.value = false
  syncLog.value.push("Sync stopped by user.")
}

const openBrowser = () => {
  if (verificationURI.value) {
    BrowserOpenURL(verificationURI.value)
  }
}

const copyCode = async () => {
  await navigator.clipboard.writeText(deviceCode.value)
  isCopied.value = true
  setTimeout(() => {
    isCopied.value = false
  }, 2000)
}

const closeAuthModal = async () => {
  showAuthModal.value = false
  // Also stop the sync process since the user aborted auth
  await stopSync()
}

</script>

<template>
  <div class="min-h-screen bg-slate-950 text-slate-100 flex flex-col font-sans text-sm selection:bg-blue-500/30">
    
    <!-- Top Navigation Bar -->
    <div class="h-14 bg-slate-900/50 backdrop-blur-md border-b border-slate-800/50 flex items-center justify-between px-5 shrink-0 z-10 sticky top-0">
      <div class="flex items-center gap-2">
        <div class="w-6 h-6 rounded bg-gradient-to-br from-blue-500 to-indigo-600 flex items-center justify-center shadow-lg shadow-blue-500/20">
          <svg class="w-4 h-4 text-white" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M13 10V3L4 14h7v7l9-11h-7z" /></svg>
        </div>
        <span class="font-bold tracking-wide text-slate-100">DriftSync</span>
      </div>
      <button 
        @click="currentTab = currentTab === 'dashboard' ? 'settings' : 'dashboard'" 
        class="w-8 h-8 flex items-center justify-center rounded-full hover:bg-slate-800 text-slate-400 hover:text-white transition-colors"
      >
        <svg v-if="currentTab === 'dashboard'" class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"></path><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"></path></svg>
        <svg v-else class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"></path></svg>
      </button>
    </div>

    <!-- Main Scrollable Area -->
    <div class="flex-1 overflow-y-auto overflow-x-hidden relative">
      
      <!-- Dashboard View -->
      <transition name="fade" mode="out-in">
        <div v-if="currentTab === 'dashboard'" class="p-6 flex flex-col h-full absolute inset-0">
          
          <!-- Hero Section -->
          <div class="flex-1 flex flex-col items-center justify-center py-8">
            <div class="relative group cursor-pointer" @click="isSyncing ? stopSync() : startSync()">
              <div v-if="isSyncing" class="absolute -inset-4 bg-indigo-500/20 rounded-full blur-xl animate-pulse"></div>
              <div 
                :class="[
                  'w-32 h-32 rounded-full flex items-center justify-center shadow-2xl transition-all duration-300 relative z-10',
                  isSyncing ? 'bg-slate-800 border-2 border-indigo-500 shadow-indigo-500/30' : 'bg-gradient-to-br from-indigo-500 to-blue-600 hover:from-indigo-400 hover:to-blue-500 shadow-blue-500/20 hover:scale-105 active:scale-95'
                ]"
              >
                <div v-if="!isSyncing" class="flex flex-col items-center gap-1">
                  <svg class="w-8 h-8 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"></path></svg>
                  <span class="text-white font-bold tracking-wide">SYNC</span>
                </div>
                <div v-else class="flex flex-col items-center gap-1">
                  <svg class="w-8 h-8 text-indigo-400 animate-spin" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
                  <span class="text-indigo-400 font-bold tracking-wide">STOP</span>
                </div>
              </div>
            </div>

            <div class="mt-8 text-center px-4">
              <p class="text-slate-400 text-sm mb-1">Local Path</p>
              <div class="bg-slate-900/80 border border-slate-800 rounded-lg px-3 py-1.5 text-xs font-mono text-blue-400 inline-block max-w-full truncate shadow-inner">
                {{ config.LocalPath || 'Not configured' }}
              </div>
            </div>
          </div>

          <!-- Logs Section -->
          <div class="mt-auto pt-4 flex flex-col h-48 shrink-0">
            <div class="flex items-center justify-between mb-2 px-1">
              <span class="text-xs font-semibold text-slate-500 uppercase tracking-wider">Activity Log</span>
            </div>
            <div ref="logContainer" @scroll="handleLogScroll" class="flex-1 bg-slate-900/50 rounded-xl border border-slate-800/80 p-3 font-mono text-[11px] text-slate-400 overflow-y-auto shadow-inner custom-scrollbar relative backdrop-blur-sm">
              <div v-if="syncLog.length === 0" class="text-slate-600 italic absolute inset-0 flex items-center justify-center">Ready to sync</div>
              <div v-for="(log, i) in syncLog" :key="i" class="py-1 border-b border-slate-800/30 last:border-0 leading-relaxed break-words" :class="{'text-emerald-400/90': log.includes('UPLOAD'), 'text-blue-400/90': log.includes('DOWNLOAD'), 'text-red-400/90': log.includes('DELETE')}">
                {{ log }}
              </div>
            </div>
          </div>
        </div>

        <!-- Setup/Onboarding View -->
        <div v-else-if="currentTab === 'setup'" class="p-4 flex flex-col h-full absolute inset-0 pb-6">
          <div class="max-w-xl mx-auto w-full flex flex-col h-full">
            
            <div class="text-center mb-5 mt-1 shrink-0">
              <h2 class="text-xl font-bold text-white tracking-wide mb-1">Welcome to DriftSync</h2>
              <p class="text-sm text-slate-400">Let's get your environment set up before syncing.</p>
            </div>

            <!-- 1. Storage Location -->
            <div class="mb-5 bg-slate-900/50 backdrop-blur-sm border border-slate-800/80 rounded-xl p-4 shadow-inner shrink-0">
              <h3 class="text-sm font-semibold text-slate-300 mb-2 flex items-center gap-2">
                <svg class="w-4 h-4 text-indigo-400" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z"></path></svg>
                1. Choose Local Sync Directory
              </h3>
              <p class="text-[11px] text-slate-500 mb-3">This is the folder where your OneDrive files will be downloaded to.</p>
              <div class="flex gap-2">
                <input v-model="config.LocalPath" type="text" readonly class="flex-1 bg-slate-950/50 border border-slate-800 rounded-lg px-3 py-2 text-slate-200 font-mono text-xs focus:outline-none transition-colors truncate cursor-default" />
                <button @click="selectDir" class="px-4 py-2 bg-slate-800 hover:bg-slate-700 text-white rounded-lg text-xs font-medium transition-colors shadow-sm whitespace-nowrap">Browse</button>
              </div>
            </div>

            <!-- 2. Selective Sync -->
            <div class="flex-1 flex flex-col min-h-0 bg-slate-900/50 backdrop-blur-sm border border-slate-800/80 rounded-xl p-4 shadow-inner">
              <div class="mb-3 shrink-0">
                <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
                  <svg class="w-4 h-4 text-indigo-400" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 002-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10"></path></svg>
                  2. Choose What To Sync
                </h3>
                <p class="text-[11px] text-slate-500 mt-1">Unchecked items will remain in the cloud but won't be downloaded to your local computer.</p>
              </div>

              <div class="flex-1 overflow-y-auto custom-scrollbar border border-slate-800/50 rounded-lg bg-slate-950/30 p-2">
                <div v-if="loadingWizardNodes" class="h-full flex flex-col items-center justify-center gap-3">
                  <svg class="w-8 h-8 text-indigo-500 animate-spin" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
                  <span class="text-slate-400 text-sm">Loading OneDrive contents...</span>
                </div>
                <div v-else class="space-y-1">
                  <div class="flex justify-start mb-2 pl-8 pt-1">
                    <label class="flex items-center gap-2 cursor-pointer group select-none">
                      <div class="relative w-4 h-4 flex items-center justify-center">
                        <input type="checkbox" v-model="wizardSelectAll" class="peer appearance-none w-4 h-4 border border-slate-600 rounded bg-slate-800/50 checked:bg-indigo-500 checked:border-indigo-500 transition-colors shadow-inner" />
                        <svg class="absolute w-3 h-3 text-white pointer-events-none opacity-0 peer-checked:opacity-100" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2.5"><path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7" /></svg>
                      </div>
                      <span class="text-xs font-medium text-slate-400 group-hover:text-indigo-400 transition-colors">Select / Deselect All</span>
                    </label>
                  </div>
                  <FolderNode
                    v-for="node in wizardRootNodes"
                    ref="setupFolderNodeRefs"
                    :key="node.path"
                    :node="node"
                    :config="config"
                    :forceState="wizardSelectAll"
                  />
                </div>
              </div>
            </div>

            <!-- Action & Advanced -->
            <div class="mt-4 shrink-0 flex flex-col gap-3">
              <button @click="completeSetup" class="w-full flex items-center justify-center gap-2 px-4 py-3.5 rounded-xl text-sm font-bold transition-all shadow-lg active:scale-[0.98] bg-gradient-to-r from-indigo-500 to-blue-600 hover:from-indigo-400 hover:to-blue-500 text-white border border-indigo-400/30 group">
                <span>Complete Setup & Start Syncing</span>
                <svg class="w-4 h-4 group-hover:translate-x-1 transition-transform" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M14 5l7 7m0 0l-7 7m7-7H3"></path></svg>
              </button>

              <div class="text-center">
                <button @click="changeWorkspaceDir" class="text-[11px] text-slate-500 hover:text-slate-300 transition-colors underline decoration-slate-600 hover:decoration-slate-400 underline-offset-2">
                  Advanced: Change Configuration Storage Location
                </button>
              </div>
            </div>
          </div>
        </div>

      <!-- Settings View -->
        <div v-else class="p-6 absolute inset-0 overflow-y-auto custom-scrollbar">
          <div class="space-y-6 pb-6">
            
            <!-- Auth -->
            <section>
              <h2 class="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-3 px-1">Authentication</h2>
              <div class="bg-slate-900/50 border border-slate-800/80 rounded-xl p-4 space-y-4 backdrop-blur-sm">
                <div>
                  <label class="block text-xs text-slate-400 mb-1.5 ml-1">Tenant</label>
                  <input v-model="config.Tenant" type="text" class="w-full bg-slate-950/50 border border-slate-800 rounded-lg px-3 py-2 text-slate-200 text-sm focus:outline-none focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500 transition-colors" placeholder="common" />
                </div>
                <div>
                  <label class="block text-xs text-slate-400 mb-1.5 ml-1">Client ID</label>
                  <input v-model="config.ClientID" type="text" class="w-full bg-slate-950/50 border border-slate-800 rounded-lg px-3 py-2 text-slate-200 text-sm focus:outline-none focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500 transition-colors" />
                </div>
              </div>
            </section>

            <!-- Storage -->
            <section>
              <h2 class="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-3 px-1">Storage</h2>
              <div class="bg-slate-900/50 border border-slate-800/80 rounded-xl p-4 space-y-4 backdrop-blur-sm">
                <div>
                  <label class="block text-xs text-slate-400 mb-1.5 ml-1">Local Directory</label>
                  <div class="flex gap-2">
                    <input v-model="config.LocalPath" type="text" class="flex-1 bg-slate-950/50 border border-slate-800 rounded-lg px-3 py-2 text-slate-200 font-mono text-xs focus:outline-none focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500 transition-colors truncate" />
                    <button @click="selectDir" class="px-3 py-2 bg-slate-800 hover:bg-slate-700 text-white rounded-lg text-sm transition-colors shadow-sm">Browse</button>
                    <button @click="openDir" class="px-3 py-2 bg-slate-800 hover:bg-slate-700 text-indigo-400 rounded-lg transition-colors shadow-sm flex items-center justify-center group" title="Open in File Explorer">
                      <svg class="w-4 h-4 group-hover:scale-110 transition-transform" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z"></path></svg>
                    </button>
                  </div>
                </div>

                <div class="pt-2">
                  <label class="block text-xs text-slate-400 mb-2 ml-1">Sync Direction</label>
                  <div class="space-y-2">
                    <label class="flex items-center justify-between p-2.5 rounded-lg bg-slate-950/30 hover:bg-slate-950/50 cursor-pointer transition-colors border border-transparent hover:border-slate-800/50">
                      <span class="text-slate-300 text-sm">Download from Cloud</span>
                      <div class="relative flex items-center">
                        <input type="checkbox" v-model="config.DownloadFromCloud" class="peer sr-only" />
                        <div class="w-9 h-5 bg-slate-800 peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-slate-300 after:border after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:bg-indigo-500 shadow-inner"></div>
                      </div>
                    </label>
                    <label class="flex items-center justify-between p-2.5 rounded-lg bg-slate-950/30 hover:bg-slate-950/50 cursor-pointer transition-colors border border-transparent hover:border-slate-800/50">
                      <span class="text-slate-300 text-sm">Upload to Cloud</span>
                      <div class="relative flex items-center">
                        <input type="checkbox" v-model="config.UploadFromLocal" class="peer sr-only" />
                        <div class="w-9 h-5 bg-slate-800 peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-slate-300 after:border after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:bg-indigo-500 shadow-inner"></div>
                      </div>
                    </label>
                  </div>
                </div>
              </div>
            </section>

            <!-- Advanced -->
            <section>
              <h2 class="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-3 px-1">Selection</h2>
              <div class="flex-1 bg-slate-900/50 rounded-xl border border-slate-800/80 overflow-y-auto custom-scrollbar p-3 relative shadow-inner">
                <div v-if="loadingWizardNodes" class="absolute inset-0 flex items-center justify-center text-slate-500 italic">
                  Loading root folders...
                </div>
                <div v-else class="space-y-1">
                  <div class="mb-3 pb-3 border-b border-slate-800/50">
                    <label class="flex items-center gap-2 cursor-pointer group px-1">
                      <div class="relative flex items-center justify-center">
                        <input type="checkbox" v-model="wizardSelectAll" class="peer appearance-none w-4 h-4 rounded border border-slate-600 bg-slate-800 checked:bg-indigo-500 checked:border-indigo-500 transition-colors" />
                        <svg class="absolute w-3 h-3 text-white pointer-events-none opacity-0 peer-checked:opacity-100" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="3"><path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7" /></svg>
                      </div>
                      <span class="text-sm font-medium text-slate-300 group-hover:text-indigo-400 transition-colors">Select All / Deselect All</span>
                    </label>
                  </div>
                  <FolderNode 
                    v-for="node in wizardRootNodes" 
                    ref="folderNodeRefs"
                    :key="node.path" 
                    :node="node" 
                    :config="config" 
                    :forceState="wizardSelectAll"
                  />
                </div>
              </div>
            </section>

            <section>
              <h2 class="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-3 px-1">Advanced</h2>
              <div class="bg-slate-900/50 border border-slate-800/80 rounded-xl p-2 backdrop-blur-sm">
                <button @click="locateConfig" class="w-full flex items-center justify-between p-3 rounded-lg hover:bg-slate-800/50 text-slate-300 transition-colors group">
                  <div class="flex items-center gap-3">
                    <div class="p-1.5 rounded-md bg-slate-800 text-slate-400 group-hover:text-indigo-400 transition-colors"><svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"></path><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"></path></svg></div>
                    <span class="text-sm">Locate config.yaml</span>
                  </div>
                  <svg class="w-4 h-4 text-slate-500 group-hover:text-slate-300 transition-colors group-hover:translate-x-0.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7"></path></svg>
                </button>
                <button @click="locateDb" class="w-full flex items-center justify-between p-3 rounded-lg hover:bg-slate-800/50 text-slate-300 transition-colors group">
                  <div class="flex items-center gap-3">
                    <div class="p-1.5 rounded-md bg-slate-800 text-slate-400 group-hover:text-indigo-400 transition-colors"><svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 7v10c0 2.21 3.582 4 8 4s8-1.79 8-4V7M4 7c0 2.21 3.582 4 8 4s8-1.79 8-4M4 7c0-2.21 3.582-4 8-4s8 1.79 8 4"></path></svg></div>
                    <span class="text-sm">Locate driftsync.db</span>
                  </div>
                  <svg class="w-4 h-4 text-slate-500 group-hover:text-slate-300 transition-colors group-hover:translate-x-0.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7"></path></svg>
                </button>
              </div>
            </section>

            <!-- Save Action -->
            <div class="mt-8 flex items-center gap-3 pt-5 border-t border-slate-800/80">
              <button @click="saveConfig" :class="[
                'flex-1 flex items-center justify-center gap-2 px-4 py-2 rounded-lg text-sm font-semibold transition-all shadow-sm active:scale-95 whitespace-nowrap',
                isSaveSuccess ? 'bg-emerald-500 hover:bg-emerald-400 text-white shadow-emerald-500/30 border border-emerald-400/50' : 'bg-indigo-500 hover:bg-indigo-400 text-white border border-indigo-400/50 hover:shadow-md hover:shadow-indigo-500/20',
                isSaving && !isSaveSuccess ? 'opacity-70 cursor-not-allowed' : ''
              ]" :disabled="isSaving">
                <svg v-if="!isSaveSuccess" class="w-4 h-4 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 7H5a2 2 0 00-2 2v9a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-3m-1 4l-3 3m0 0l-3-3m3 3V4"></path></svg>
                <svg v-else class="w-4 h-4 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7"></path></svg>
                {{ isSaveSuccess ? 'Saved!' : 'Save Configuration' }}
              </button>
              <button @click="openStartOverModal" class="flex-1 flex items-center justify-center gap-2 px-4 py-2 rounded-lg text-sm font-semibold transition-all shadow-sm hover:shadow-md hover:shadow-red-900/20 active:scale-95 bg-red-950/40 hover:bg-red-600 text-red-400 hover:text-white border border-red-900/50 group whitespace-nowrap">
                <svg class="w-4 h-4 shrink-0 group-hover:animate-pulse" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"></path></svg>
                Start Over
              </button>
            </div>
            
          </div>
        </div>
      </transition>
    </div>

    <!-- Auth Modal (Glassmorphism overlay) -->
    <transition name="fade">
      <div v-if="showAuthModal" @click.self="closeAuthModal" class="absolute inset-0 bg-slate-950/60 backdrop-blur-md flex items-center justify-center p-4 z-50">
        <div class="bg-slate-900/90 border border-slate-700/50 rounded-2xl shadow-2xl max-w-sm w-full overflow-hidden animate-in zoom-in-95 duration-200">
          <div class="p-6 text-center space-y-5">
            <div class="w-14 h-14 bg-indigo-500/10 rounded-full flex items-center justify-center mx-auto mb-2 border border-indigo-500/20">
              <svg class="w-7 h-7 text-indigo-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z" />
              </svg>
            </div>
            <h2 class="text-xl font-bold text-white">Authentication</h2>
            <div class="space-y-5 text-left">
              <div>
                <p class="text-xs text-slate-400 mb-2">1. Click to open Microsoft authorization page:</p>
                <button @click="openBrowser" class="text-left w-full bg-slate-950/50 hover:bg-slate-800 text-indigo-400 px-4 py-3 rounded-xl border border-slate-800 transition-colors break-all underline decoration-indigo-500/30 underline-offset-4 text-xs leading-relaxed shadow-inner">
                  {{ verificationURI }}
                </button>
              </div>

              <div>
                <p class="text-xs text-slate-400 mb-2">2. Paste this code to authorize DriftSync:</p>
                <div class="bg-slate-950/80 rounded-xl p-3 border border-slate-800 flex items-center justify-between shadow-inner">
                  <div class="text-xl font-mono font-bold text-white tracking-widest pl-1">{{ deviceCode }}</div>
                  <button @click="copyCode" :class="['px-3 py-2 rounded-lg font-medium text-xs transition-colors shadow-md active:scale-95 flex items-center gap-1.5', isCopied ? 'bg-emerald-500/20 text-emerald-400 border border-emerald-500/30 hover:bg-emerald-500/30' : 'bg-indigo-600 hover:bg-indigo-500 text-white shadow-indigo-900/20']">
                    <template v-if="!isCopied">
                      <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z"></path></svg>
                      Copy
                    </template>
                    <template v-else>
                      <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7"></path></svg>
                      Copied
                    </template>
                  </button>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </transition>

    <!-- Start Over Modal -->
    <transition name="fade">
      <div v-if="showStartOverModal" class="absolute inset-0 bg-slate-950/80 backdrop-blur-md flex items-center justify-center p-4 z-[80]">
        <div class="bg-slate-900 border border-red-900/50 rounded-2xl shadow-[0_0_40px_-10px_rgba(239,68,68,0.3)] max-w-2xl w-full overflow-hidden animate-in zoom-in-95 duration-200">
          <div class="p-6">
            <!-- Header -->
            <div class="flex items-center gap-4 mb-5">
              <div class="w-12 h-12 bg-red-500/10 rounded-full flex items-center justify-center shrink-0 border border-red-500/20">
                <svg class="w-6 h-6 text-red-500" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"></path></svg>
              </div>
              <h3 class="text-lg font-bold text-white">DANGER: START OVER</h3>
            </div>
            
            <!-- Content -->
            <div class="text-sm text-slate-300 leading-relaxed space-y-4">
              <p>This action will permanently delete all synced files in your local directory, along with your configuration and sync database.</p>
              
              <div class="bg-slate-950 rounded border border-red-900/30 p-3 flex flex-col w-full shadow-inner">
                <p class="text-xs font-semibold text-red-400 uppercase tracking-wider mb-2 shrink-0">The following paths will be PERMANENTLY DELETED:</p>
                <div class="overflow-x-auto custom-scrollbar pb-1.5 space-y-1">
                  <p v-for="p in pathsToDelete" :key="p" class="font-mono text-[11px] text-slate-400 whitespace-nowrap pr-4">• {{ p }}</p>
                </div>
              </div>

              <div class="space-y-1">
                <p class="font-semibold text-white">The application is not responsible for any data loss. Please back up your local data manually.</p>
                <p class="text-[11px] text-slate-400 italic">Note: If data is unexpectedly lost, you may attempt to recover it from Microsoft OneDrive's cloud recycle bin.</p>
              </div>
            </div>

            <div class="mt-6">
              <label class="flex items-start gap-3 p-3 rounded bg-red-500/5 border border-red-500/20 cursor-pointer hover:bg-red-500/10 transition-colors select-none">
                <div class="mt-0.5">
                  <input type="checkbox" v-model="agreeToStartOver" class="w-4 h-4 text-red-600 bg-slate-900 border-red-900 focus:ring-red-600 rounded" />
                </div>
                <span class="text-sm font-medium text-red-200">I agree to permanently delete these files.</span>
              </label>
            </div>
          </div>
          
          <div class="p-4 bg-slate-950/50 border-t border-red-900/30 flex gap-3 justify-end">
            <button @click="showStartOverModal = false" :disabled="startOverLoading" class="px-4 py-2 rounded-lg text-sm font-medium text-slate-300 hover:bg-slate-800 transition-colors disabled:opacity-50">
              Cancel
            </button>
            <button @click="handleStartOver" :disabled="!agreeToStartOver || startOverLoading" class="bg-red-600 hover:bg-red-500 text-white px-4 py-2 rounded-lg text-sm font-medium transition-colors shadow-lg shadow-red-900/20 disabled:opacity-50 disabled:cursor-not-allowed flex items-center gap-2">
              <svg v-if="startOverLoading" class="w-4 h-4 animate-spin" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
              <span>I understand, Delete Everything</span>
            </button>
          </div>
        </div>
      </div>
    </transition>

  </div>
</template>

<style>
/* Animations and Scrollbar Customization */
@keyframes fadeIn { from { opacity: 0; } to { opacity: 1; } }
@keyframes zoomIn { from { opacity: 0; transform: scale(0.95); } to { opacity: 1; transform: scale(1); } }
.animate-in.fade-in { animation: fadeIn 0.2s ease-out; }
.animate-in.zoom-in-95 { animation: zoomIn 0.2s ease-out; }

.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s ease;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

/* Custom Scrollbar for sleek UI */
.custom-scrollbar::-webkit-scrollbar {
  width: 6px;
  height: 6px;
}
.custom-scrollbar::-webkit-scrollbar-track {
  background: transparent;
}
.custom-scrollbar::-webkit-scrollbar-thumb {
  background-color: rgba(71, 85, 105, 0.4);
  border-radius: 10px;
}
.custom-scrollbar::-webkit-scrollbar-thumb:hover {
  background-color: rgba(71, 85, 105, 0.8);
}
.custom-scrollbar::-webkit-scrollbar-corner {
  background: transparent;
}
</style>
