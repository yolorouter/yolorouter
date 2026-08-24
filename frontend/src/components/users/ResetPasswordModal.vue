<!-- frontend/src/components/users/ResetPasswordModal.vue
     The bootstrap administrator replacing another local account's
     password. Same password UX as the create dialog (hand-typed or
     generated, visible in the field); the reset kills every live session
     of the target, so the operator hands the new password over out of
     band and the owner signs in fresh. -->
<template>
  <ModalDrawer
    v-model:show="show"
    :title="t('users.resetPasswordTitle')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('users.resetPassword')"
    :loading="submitting"
    @confirm="onReset"
    @after-leave="resetForm"
  >
    <n-alert type="warning" :show-icon="false" class="reset-warning">
      {{ t('users.resetPasswordNotice', { name: username }) }}
    </n-alert>
    <n-form
      ref="formRef"
      require-mark-placement="left"
      :model="form"
      :rules="rules"
      label-placement="top"
    >
      <n-form-item path="password">
        <template #label>
          <HelpLabel :tip="t('users.resetPasswordNew_tip')">{{ t('users.resetPasswordNew') }}</HelpLabel>
        </template>
        <n-input
          v-model:value="form.password"
          type="password"
          show-password-on="click"
          :maxlength="72"
        >
          <template #suffix>
            <n-button size="small" quaternary :disabled="!form.password" @click="copy(form.password)">
              {{ copied ? t('common.copied') : t('common.copy') }}
            </n-button>
            <n-button size="small" quaternary @click="fillGeneratedPassword">{{ t('users.generatePassword') }}</n-button>
          </template>
        </n-input>
      </n-form-item>
    </n-form>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NForm, NFormItem, NInput, useMessage, type FormInst, type FormRules } from 'naive-ui'

import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import { displayMessage } from '../../api/client'
import { resetUserPassword } from '../../api/users'
import { passwordStrengthRule } from '../../utils/authValidators'
import { generatePassword } from '../../utils/passwordGenerator'
import { useCopyFeedback } from '../../composables/useCopyFeedback'

// ModalDrawer wants a v-model:show and the parent speaks v-model:show too,
// so one defineModel bridges both without a hand-rolled computed.
const show = defineModel<boolean>('show', { required: true })
const props = defineProps<{ userId: number; username: string }>()
const emit = defineEmits<{ reset: [] }>()

const { t } = useI18n()
const message = useMessage()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)

const form = reactive({ password: '' })
const rules = computed<FormRules>(() => ({
  password: passwordStrengthRule(t),
}))

function fillGeneratedPassword() {
  form.password = generatePassword()
}

// Shared copy-button feedback (label flip + failure toast).
const { copied, copy } = useCopyFeedback()

function resetForm() {
  form.password = ''
  copied.value = false
  formRef.value?.restoreValidation()
}

async function onReset() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    await resetUserPassword(props.userId, form.password)
    message.success(t('users.resetPasswordSuccess'))
    show.value = false
    emit('reset')
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.reset-warning {
  margin-bottom: var(--space-3);
}
</style>
