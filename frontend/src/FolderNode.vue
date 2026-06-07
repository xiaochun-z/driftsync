<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { GetRemoteItems } from '../wailsjs/go/main/App'

const props = defineProps<{
  node: any;
  config?: any;
  forceState?: boolean;
}>()

const emit = defineEmits(['toggle'])

const expanded = ref(false)
const loading = ref(false)
const children = ref<any[]>([])

const getInitialState = (path: string, parentState?: number): number => {
  if (parentState === 0) return 0;
  
  if (!props.config || !props.config.Sync || !props.config.Sync.Exclude) {
    return 1;
  }
  const excludes: string[] = props.config.Sync.Exclude;
  
  if (excludes.includes(path)) {
    return 0;
  }
  
  const prefix = path === '/' ? '/' : path + '/';
  const hasExcludedChildren = excludes.some(ex => ex.startsWith(prefix));
  if (hasExcludedChildren) {
    return 2;
  }
  
  return 1;
}

onMounted(() => {
  if (props.node.state === undefined) {
    props.node.state = getInitialState(props.node.path)
  }
})

watch(() => props.forceState, (newVal) => {
  if (newVal !== undefined) {
    forceStateAction(newVal ? 1 : 0)
  }
})

const toggleExpand = async () => {
  if (!props.node.isFolder) return
  
  if (!expanded.value && children.value.length === 0) {
    loading.value = true
    try {
      const items = await GetRemoteItems(props.node.path)
      children.value = items.map((it: any) => {
        const childPath = props.node.path === '/' ? `/${it.name}` : `${props.node.path}/${it.name}`
        return {
          id: it.id,
          name: it.name,
          path: childPath,
          state: getInitialState(childPath, props.node.state),
          isFolder: !!it.folder
        }
      })
    } catch (e) {
      console.error(e)
    } finally {
      loading.value = false
    }
  }
  expanded.value = !expanded.value
}

// Checkbox state: 1 (checked), 0 (unchecked), 2 (indeterminate)
const checkboxIcon = computed(() => {
  if (props.node.state === 1) {
    return `<svg class="w-3.5 h-3.5 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M5 13l4 4L19 7"></path></svg>`
  } else if (props.node.state === 2) {
    return `<svg class="w-3.5 h-3.5 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M20 12H4"></path></svg>`
  }
  return ''
})

const checkboxClass = computed(() => {
  if (props.node.state === 1) return 'bg-indigo-500 border-indigo-500'
  if (props.node.state === 2) return 'bg-indigo-500 border-indigo-500 opacity-80'
  return 'bg-slate-900 border-slate-700'
})

const onToggle = () => {
  // Toggle: if indeterminate or unchecked -> checked. if checked -> unchecked.
  const newState = props.node.state === 1 ? 0 : 1
  props.node.state = newState
  
  // Propagate to all loaded children
  setChildrenState(children.value, newState)
  
  // Emit to parent to recalculate
  emit('toggle')
}

const setChildrenState = (nodes: any[], state: number) => {
  nodes.forEach(n => {
    n.state = state
    if (n.children && n.children.length > 0) {
      setChildrenState(n.children, state)
    }
  })
}

const childRefs = ref<any[]>([])

// When a child toggles, we need to recalculate our state
const onChildToggle = () => {
  if (children.value.length > 0) {
    const allChecked = children.value.every(c => c.state === 1)
    const allUnchecked = children.value.every(c => c.state === 0)
    
    if (allChecked) {
      props.node.state = 1
    } else if (allUnchecked) {
      props.node.state = 0
    } else {
      props.node.state = 2 // indeterminate
    }
  }
  // Propagate upward
  emit('toggle')
}

const getExcludeRules = (): string[] => {
  const rules: string[] = []
  if (props.node.state === 0) {
    // Completely unchecked
    rules.push(props.node.path)
  } else if (props.node.state === 2) {
    // Indeterminate: check children
    if (childRefs.value) {
      childRefs.value.forEach((c: any) => {
        rules.push(...c.getExcludeRules())
      })
    }
  }
  return rules
}

const forceStateAction = (newState: number) => {
  props.node.state = newState
  if (children.value.length > 0) {
    setChildrenState(children.value, newState)
  }
  if (childRefs.value) {
    childRefs.value.forEach((c: any) => c.forceState(newState))
  }
}

// Expose forceState and getExcludeRules for parent calls
defineExpose({ getExcludeRules, forceState: forceStateAction })
</script>

<template>
  <div class="select-none">
    <div class="flex items-center gap-2 py-1.5 hover:bg-slate-800/50 rounded px-1 transition-colors group">
      <!-- Expander Arrow / File Icon -->
      <button v-if="node.isFolder" @click="toggleExpand" class="w-5 h-5 flex items-center justify-center rounded hover:bg-slate-700 text-slate-400 transition-colors">
        <svg v-if="loading" class="w-3 h-3 animate-spin" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
        <svg v-else class="w-3.5 h-3.5 transition-transform duration-200" :class="{'rotate-90': expanded}" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7"></path></svg>
      </button>
      <div v-else class="w-5 h-5 flex items-center justify-center text-slate-500">
        <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M7 21h10a2 2 0 002-2V9.414a1 1 0 00-.293-.707l-5.414-5.414A1 1 0 0012.586 3H7a2 2 0 00-2 2v14a2 2 0 002 2z"></path></svg>
      </div>

      <!-- Checkbox -->
      <div 
        @click="onToggle"
        class="w-4 h-4 rounded border flex items-center justify-center cursor-pointer transition-colors shadow-inner"
        :class="checkboxClass"
        v-html="checkboxIcon"
      ></div>

      <!-- Label -->
      <span @click="onToggle" class="text-sm text-slate-200 cursor-pointer truncate flex-1">{{ node.name }}</span>
    </div>

    <!-- Children -->
    <div v-show="expanded" class="ml-5 border-l border-slate-800/50 pl-1 mt-0.5">
      <FolderNode 
        v-for="child in children" 
        :key="child.id" 
        :node="child"
        @toggle="onChildToggle"
        ref="childRefs"
      />
    </div>
  </div>
</template>
