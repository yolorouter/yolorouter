<!-- frontend/src/components/users/EditProfileModal.vue
     The bootstrap administrator rewriting another account's display name
     and email — directory information only. Both fields are prefilled
     with the current values so the edit is a minimal diff, and saving
     never disturbs the target's login (no credential or permission
     semantics changed). -->
<template>
  <ModalDrawer
    v-model:show="show"
    :title="t('users.editProfileTitle')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('common.save')"
    :loading="saving"
    @confirm="onSave"
    @after-leave="resetForm"
  >
    <n-form
      ref="formRef"
      require-mark-placement="left"
      :model="form"
      :rules="rules"
      label-placement="top"
    >
      <n-form-item path="display_name">
        <template #label>
          <HelpLabel :tip="t('users.displayNameField_tip')">{{ t('users.displayNameField') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.display_name" :maxlength="128" />
      </n-form-item>
      <n-form-item path="email">
        <template #label>
          <HelpLabel :tip="t('users.emailField_tip')">{{ t('users.emailField') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.email" :maxlength="255" />
      </n-form-item>
    </n-form>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NForm, NFormItem, NInput, useMessage, type FormInst, type FormRules } from 'naive-ui'

import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import { displayMessage } from '../../api/client'
import { updateUserProfile } from '../../api/users'
import { emailFormatRule } from '../../utils/authValidators'

// ModalDrawer wants a v-model:show and the parent speaks v-model:show too,
// so one defineModel bridges both without a hand-rolled computed.
const show = defineModel<boolean>('show', { required: true })
const props = defineProps<{
  userId: number
  displayName: string
  email: string
}>()
const emit = defineEmits<{ saved: [] }>()

const { t } = useI18n()
const message = useMessage()

const formRef = ref<FormInst | null>(null)
const saving = ref(false)

function initialForm() {
  return { display_name: props.displayName, email: props.email }
}
const form = reactive(initialForm())

// The target can change between openings (one modal, many rows); re-seed
// the form from the current props every time it opens.
watch(show, (v) => {
  if (v) Object.assign(form, initialForm())
})

const rules = computed<FormRules>(() => ({
  email: emailFormatRule(t),
}))

function resetForm() {
  formRef.value?.restoreValidation()
}

async function onSave() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  saving.value = true
  try {
    // Both fields always travel: the form is prefilled, so what it holds
    // IS the intended full profile — a cleared input means "clear the
    // field", not "leave it alone". Field-skipping stays an API-level
    // contract for other clients.
    await updateUserProfile(props.userId, {
      display_name: form.display_name,
      email: form.email,
    })
    message.success(t('users.editProfileSuccess'))
    show.value = false
    emit('saved')
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    saving.value = false
  }
}
</script>
