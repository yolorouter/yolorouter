# DingTalk / Feishu login setup

[中文版](dingtalk-feishu-login_zh.md)

YoloRouter members can sign in with their work DingTalk or Feishu account. The
first successful sign-in creates the member account automatically — no invites,
no provisioning. The account identity is the provider's stable user id (the
`unionId` / `union_id`), so renaming an account upstream never breaks sign-in
here, and two different people who happen to share a display name still get two
separate accounts.

Both setups follow the same shape: create an app on the provider's developer
console, register the router's callback URL there, then add a provider in the
router console with the built-in preset.

## Prerequisites

- `server.external_url` in `configs/config.yaml` must be set to the origin
  browsers actually use to reach this YoloRouter instance (scheme, host, and
  port). Every callback URL is derived from it, and both providers match it
  character-for-character.
- Access to the router's admin console (System Settings → Login Providers).
- Admin access to your company's DingTalk / Feishu developer console.

The exact callback URL for each provider is shown inside the router's provider
form — copy it from there rather than typing it by hand.

## Feishu

1. **Create the app.** On [open.feishu.cn](https://open.feishu.cn) (developer
   console) create a **custom app** (企业自建应用). Under "Credentials & Basic
   Info" (凭证与基础信息) copy the **App ID** and **App Secret**.
2. **Register the redirect URL.** Under "Security Settings" (安全设置) →
   redirect URLs, add the callback URL shown in the router's provider form:
   `http(s)://YOUR-HOST/oauth/callback/feishu`. It is matched exactly, so use
   the copy button.
3. **(Optional) user emails.** The user's email is only returned when the app
   has the *Get user email info* (`contact:user.email:readonly`) permission.
   Without it the auto-created account simply has an empty email — sign-in is
   unaffected. Note Feishu marks emails as admin-imported and not verified by
   the user, which is why YoloRouter never uses email as an identity.
4. **Publish the app version** so tenant members can use it.
5. **Add the provider in the router.** Admin console → Login Providers → Add →
   setup method **Feishu** → paste the App ID / App Secret → Save. Everything
   else (endpoints, token protocol, field mapping) is pre-filled by the preset.
6. **Test.** Sign out, open the login page, click the Feishu button, approve
   the consent page. You should land in the console as a new member account.

**Who can sign in:** only members of the Feishu tenant this app belongs to.
The app is the admission gate — narrow its availability range (可用范围) in the
Feishu admin console if you want to limit sign-in to part of the organization.

## DingTalk

1. **Create the app.** On [open.dingtalk.com](https://open.dingtalk.com) →
   App Development → **internal enterprise app** (企业内部应用), e.g. an H5
   micro-app. Under "Credentials & Basic Info" copy the **AppKey** and
   **AppSecret**.
2. **Register the redirect URL.** App → "Security Settings" (安全设置) →
   **Redirect URL (callback domain)** — add the full URL from the router's
   provider form, protocol and port included:
   `http(s)://YOUR-HOST:PORT/oauth/callback/dingtalk`. DingTalk matches it
   exactly.
3. **Grant the userinfo permission (required).** App → "Permission Management"
   (权限管理) → search "个人信息" → apply for **Get user personal information
   from the address book** (获取用户通讯录个人信息). Without it, sign-in fails
   with a 403 at the last step.
4. **Publish the app version.** Changes to redirect URLs and permissions only
   take effect after a release.
5. **Add the provider in the router.** Login Providers → Add → setup method
   **DingTalk** → paste the AppKey / AppSecret → Save. The preset carries the
   QR-login endpoints, the JSON token exchange, DingTalk's custom userinfo
   header, and PKCE off — leave the advanced section untouched.
6. **Test.** Login page → DingTalk button → a QR page opens on
   `login.dingtalk.com` → scan with the mobile DingTalk app and confirm the
   consent page. The account is created on the first successful sign-in.

**Who can sign in:** members of the DingTalk organization the app belongs to,
within the app's visibility range.

## Notes

- **Usernames vs display names.** The local username is derived from the
  profile name and folded to lowercase letters, digits, and dashes; a profile
  name containing no Latin letters or digits at all (e.g. 张伟) falls back to
  `<provider>-user`, suffixed on collision. The display name keeps the
  original spelling. Identity never depends on either — it is the provider's
  union id.
- **Accounts are never merged.** Each provider account maps to exactly one
  local account. If the same human signs in once through Feishu and once
  through DingTalk, that is two accounts by design; there is no auto-linking
  by name or email.
- **Manual providers.** The presets are ordinary providers with pre-filled
  values. "Manual setup" exposes the same knobs (token request encoding, JSON
  field naming, custom userinfo auth header, PKCE toggle, extra authorize
  parameters) for any other identity provider.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| Feishu authorize page rejects with "email openid profile 有误" | The provider's *scopes* field carries OIDC-style scope names Feishu does not accept | Edit the provider, clear the **Scopes** field (leave it empty), save |
| Feishu sign-in fails after consent with "PKCE code challenge failed" in the router log | Authorize and token endpoints from different Feishu API generations were mixed manually | Use the preset's endpoint pair as-is |
| DingTalk sign-in fails after the consent page; router log shows `status 403` | The app is missing the *Get user personal information* permission | Grant it in Permission Management and publish the app again |
| DingTalk QR page shows no permission, or nothing happens after scanning | Redirect URL mismatch (protocol/host/port must match exactly), or the change has not been published yet | Re-copy the URL from the router form; publish the app version |
| Account has an empty email | Expected without the provider's sensitive email field permission | Optional: grant it per provider; existing accounts are not back-filled |

When sign-in fails, the router's server log carries the provider's raw error
body for the token and userinfo steps — grep for `oauth: callback failed`.
