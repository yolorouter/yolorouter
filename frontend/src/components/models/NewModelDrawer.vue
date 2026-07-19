<!-- frontend/src/components/models/NewModelDrawer.vue -->
<template>
  <n-drawer :show="show" width="420" @update:show="onUpdateShow">
    <n-drawer-content :title="t('models.createButton')" closable>
      <n-form ref="formRef" :model="form" :rules="rules">
        <n-form-item path="name" :label="t('models.name')">
          <n-input v-model:value="form.name" :placeholder="t('models.nameHint')" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="onUpdateShow(false)">{{ t('models.cancel') }}</n-button>
          <n-button type="primary" :loading="submitting" @click="onSubmit">{{ t('models.save') }}</n-button>
        </n-space>
      </template>
    </n-drawer-content>
  </n-drawer>
</template>

<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { modelNameRule } from '../../utils/modelValidators'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ 'update:show': [boolean] }>()

const { t } = useI18n()
const message = useMessage()
const router = useRouter()
const store = useModelsStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const form = reactive({ name: '' })
const rules: FormRules = { name: modelNameRule(t) }

watch(
  () => props.show,
  (visible) => {
    if (visible) form.name = ''
  },
)

function onUpdateShow(value: boolean) {
  emit('update:show', value)
}

async function onSubmit() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    const created = await store.create(form.name)
    onUpdateShow(false)
    // PRD §6.3.4: saving a model auto-navigates into its detail page.
    router.push(`/models/${created.id}`)
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>
