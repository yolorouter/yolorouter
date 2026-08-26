<!-- frontend/src/components/users/CreateUserModal.vue
     Provision a local password member from the console.

     The admin hands the initial password to the user out of band, so the
     form keeps the chosen/generated password visible — no one-time-reveal
     ceremony. Unlike an API key, the server stores only a hash and the admin
     is expected to know what they typed. -->
<template>
  <ModalDrawer
    v-model:show="show"
    :title="t('users.createTitle')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('users.createButton')"
    :loading="creating"
    @confirm="onCreate"
    @after-leave="resetForm"
  >
    <n-form
      ref="formRef"
      require-mark-placement="left"
      :model="form"
      :rules="rules"
      label-placement="top"
    >
      <n-form-item path="username">
        <template #label>
          <HelpLabel :tip="t('users.createUsername_tip')">{{ t('users.createUsername') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.username" :maxlength="32" />
      </n-form-item>
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
      <n-form-item path="password">
        <template #label>
          <HelpLabel :tip="t('users.createPassword_tip')">{{ t('users.createPassword') }}</HelpLabel>
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
import { createUser } from '../../api/users'
import { emailFormatRule, passwordStrengthRule, usernameFormatRule } from '../../utils/authValidators'
import { generatePassword } from '../../utils/passwordGenerator'
import { useCopyFeedback } from '../../composables/useCopyFeedback'

const emit = defineEmits<{ created: [] }>()

// ModalDrawer wants a v-model:show and the parent speaks v-model:show too,
// so one defineModel bridges both without a hand-rolled computed.
const show = defineModel<boolean>('show', { required: true })

const { t } = useI18n()
const message = useMessage()

const formRef = ref<FormInst | null>(null)
const creating = ref(false)

function initialForm() {
  return { username: '', display_name: '', email: '', password: '' }
}
const form = reactive(initialForm())

// Both rules mirror the backend's binding tags character for character —
// they live in authValidators so the two stay in step. The email rule is
// a practical typo check; the backend's validator is the authority.
const rules = computed<FormRules>(() => ({
  username: usernameFormatRule(t),
  email: emailFormatRule(t),
  password: passwordStrengthRule(t),
}))

function fillGeneratedPassword() {
  form.password = generatePassword()
}

// Shared copy-button feedback (label flip + failure toast).
const { copied, copy } = useCopyFeedback()

function resetForm() {
  Object.assign(form, initialForm())
  copied.value = false
  formRef.value?.restoreValidation()
}

async function onCreate() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  creating.value = true
  try {
    await createUser({
      username: form.username,
      display_name: form.display_name || undefined,
      email: form.email || undefined,
      password: form.password,
    })
    message.success(t('users.createSuccess'))
    show.value = false
    emit('created')
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    creating.value = false
  }
}
</script>
