import { Component, inject, OnInit, signal } from '@angular/core';
import { CommonModule } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { finalize } from 'rxjs';
import { AccountService } from '../../services/account.service';
import { Account, ProviderPreset, VerificationResult } from '../../models/account.model';

@Component({
  selector: 'app-account-form',
  standalone: true,
  imports: [CommonModule, FormsModule, RouterLink],
  template: `
    <div class="container">
      <div class="page-header">
        <div>
          <h2>{{ isEditMode ? 'Modify Email Account' : 'Add New Email Account' }}</h2>
          <p class="page-subtitle">
            Configure mail server credentials and connection settings securely.
          </p>
        </div>
        <a routerLink="/" class="btn btn-secondary">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
            <line x1="19" y1="12" x2="5" y2="12"/>
            <polyline points="12 19 5 12 12 5"/>
          </svg>
          Back to Accounts
        </a>
      </div>

      <!-- Alerts -->
      <div *ngIf="errorMessage()" class="alert alert-danger">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18">
          <circle cx="12" cy="12" r="10"/>
          <line x1="12" y1="8" x2="12" y2="12"/>
          <line x1="12" y1="16" x2="12.01" y2="16"/>
        </svg>
        <span>{{ errorMessage() }}</span>
      </div>

      <!-- Verification Result Alert -->
      <div *ngIf="verificationResult() as result" [ngClass]="['alert', result.success ? 'alert-success' : 'alert-danger']">
        <div class="verification-content">
          <div class="verification-header">
            <svg *ngIf="result.success" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18">
              <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/>
              <polyline points="22 4 12 14.01 9 11.01"/>
            </svg>
            <svg *ngIf="!result.success" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18">
              <circle cx="12" cy="12" r="10"/>
              <line x1="12" y1="8" x2="12" y2="12"/>
              <line x1="12" y1="16" x2="12.01" y2="16"/>
            </svg>
            <strong>{{ result.success ? 'Connection Verified Successfully' : 'Connection Verification Failed' }}</strong>
          </div>
          <div class="verification-details">
            <div class="verify-item">
              <span class="verify-badge" [ngClass]="result.imap.success ? 'badge-green' : 'badge-danger'">IMAP:</span>
              <span>{{ result.imap.success ? (result.imap.message || 'Connected & Authenticated') : result.imap.error }}</span>
            </div>
            <div class="verify-item">
              <span class="verify-badge" [ngClass]="result.smtp.success ? 'badge-green' : 'badge-danger'">SMTP:</span>
              <span>{{ result.smtp.success ? (result.smtp.message || 'Connected & Authenticated') : result.smtp.error }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Loading Account Details -->
      <div *ngIf="loadingAccount()" class="loading-state">
        <div class="spinner"></div>
        <p>Loading account details...</p>
      </div>

      <form *ngIf="!loadingAccount()" (ngSubmit)="saveAccount()" #accountForm="ngForm" class="form-wrapper">
        <!-- Preset Selector (New Account) -->
        <div *ngIf="!isEditMode" class="card preset-card">
          <h3 class="card-section-title">Quick Presets</h3>
          <p class="section-help">Choose a provider to pre-fill common server hosts and ports:</p>
          <div class="preset-buttons">
            <button
              *ngFor="let preset of presets"
              type="button"
              class="btn btn-secondary preset-btn"
              (click)="applyPreset(preset)"
            >
              {{ preset.name }}
            </button>
          </div>
          <div *ngIf="selectedPresetInstructions" class="preset-instructions">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
              <circle cx="12" cy="12" r="10"/>
              <line x1="12" y1="16" x2="12" y2="12"/>
              <line x1="12" y1="8" x2="12.01" y2="8"/>
            </svg>
            <span>{{ selectedPresetInstructions }}</span>
          </div>
        </div>

        <!-- General Info Card -->
        <div class="card">
          <h3 class="card-section-title">General Information</h3>
          
          <div class="form-row">
            <div class="form-group flex-1">
              <label class="form-label" for="accountName">
                Account Name <span class="required">*</span>
              </label>
              <input
                id="accountName"
                type="text"
                class="form-input"
                [(ngModel)]="account.name"
                name="name"
                required
                placeholder="e.g. Work Email, Personal Gmail"
                (ngModelChange)="onNameChange()"
              />
              <div class="form-help">A friendly name for identifying this account.</div>
            </div>

            <div class="form-group flex-1">
              <label class="form-label" for="accountId">
                Account ID <span class="required" *ngIf="!isEditMode">*</span>
              </label>
              <input
                id="accountId"
                type="text"
                class="form-input"
                [(ngModel)]="account.id"
                name="id"
                required
                [disabled]="isEditMode"
                placeholder="e.g. work, personal, primary"
              />
              <div class="form-help">Unique identifier used by EmailMCP for this account.</div>
            </div>
          </div>
        </div>

        <!-- IMAP Settings Card -->
        <div class="card">
          <div class="section-header-with-badge">
            <h3 class="card-section-title">Incoming Mail (IMAP)</h3>
            <span class="badge badge-blue">Receiving & Search</span>
          </div>
          <p class="section-help">Used to fetch mailboxes, search messages, and download attachments.</p>

          <div class="form-row">
            <div class="form-group flex-2">
              <label class="form-label" for="imapHost">
                IMAP Host <span class="required">*</span>
              </label>
              <input
                id="imapHost"
                type="text"
                class="form-input"
                [(ngModel)]="account.imap_host"
                name="imap_host"
                required
                placeholder="imap.example.com"
              />
            </div>

            <div class="form-group flex-1">
              <label class="form-label" for="imapPort">
                Port <span class="required">*</span>
              </label>
              <input
                id="imapPort"
                type="number"
                class="form-input"
                [(ngModel)]="account.imap_port"
                name="imap_port"
                required
                placeholder="993"
              />
            </div>
          </div>

          <div class="form-row">
            <div class="form-group flex-1">
              <label class="form-label" for="imapUsername">
                Username / Email <span class="required">*</span>
              </label>
              <input
                id="imapUsername"
                type="text"
                class="form-input"
                [(ngModel)]="account.imap_username"
                name="imap_username"
                required
                placeholder="user@example.com"
                (ngModelChange)="onUsernameChange()"
              />
            </div>

            <div class="form-group flex-1">
              <label class="form-label" for="imapPassword">
                IMAP Password <span class="required" *ngIf="!isEditMode">*</span>
              </label>
              <div class="password-input-wrapper">
                <input
                  id="imapPassword"
                  [type]="showImapPassword ? 'text' : 'password'"
                  class="form-input"
                  [(ngModel)]="account.imap_password"
                  name="imap_password"
                  [placeholder]="isEditMode ? 'Leave blank to keep current password' : 'Enter account or app password'"
                  autocomplete="new-password"
                />
                <button
                  type="button"
                  class="btn-toggle-password"
                  (click)="showImapPassword = !showImapPassword"
                  title="Toggle password visibility"
                >
                  <svg *ngIf="!showImapPassword" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                    <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/>
                    <circle cx="12" cy="12" r="3"/>
                  </svg>
                  <svg *ngIf="showImapPassword" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                    <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/>
                    <line x1="1" y1="1" x2="23" y2="23"/>
                  </svg>
                </button>
              </div>
              <div class="form-help">
                {{ isEditMode ? 'Only enter if you wish to change the existing stored password.' : 'Enter your email account password or app-specific password.' }}
              </div>
            </div>
          </div>

          <div class="form-group">
            <label class="form-checkbox-label">
              <input
                type="checkbox"
                [(ngModel)]="account.imap_use_tls"
                name="imap_use_tls"
              />
              <span>Use TLS / SSL encryption (recommended, port 993)</span>
            </label>
          </div>
        </div>

        <!-- SMTP Settings Card -->
        <div class="card">
          <div class="section-header-with-badge">
            <h3 class="card-section-title">Outgoing Mail (SMTP)</h3>
            <span class="badge badge-blue">Sending</span>
          </div>
          <p class="section-help">Used to send emails, draft replies, and deliver attachments.</p>

          <div class="form-row">
            <div class="form-group flex-2">
              <label class="form-label" for="smtpHost">
                SMTP Host <span class="required">*</span>
              </label>
              <input
                id="smtpHost"
                type="text"
                class="form-input"
                [(ngModel)]="account.smtp_host"
                name="smtp_host"
                required
                placeholder="smtp.example.com"
              />
            </div>

            <div class="form-group flex-1">
              <label class="form-label" for="smtpPort">
                Port <span class="required">*</span>
              </label>
              <input
                id="smtpPort"
                type="number"
                class="form-input"
                [(ngModel)]="account.smtp_port"
                name="smtp_port"
                required
                placeholder="587"
              />
            </div>
          </div>

          <div class="form-row">
            <div class="form-group flex-1">
              <label class="form-label" for="smtpUsername">
                SMTP Username (Optional)
              </label>
              <input
                id="smtpUsername"
                type="text"
                class="form-input"
                [(ngModel)]="account.smtp_username"
                name="smtp_username"
                placeholder="Leave blank to use IMAP username"
              />
            </div>

            <div class="form-group flex-1">
              <label class="form-label" for="smtpPassword">
                SMTP Password (Optional)
              </label>
              <div class="password-input-wrapper">
                <input
                  id="smtpPassword"
                  [type]="showSmtpPassword ? 'text' : 'password'"
                  class="form-input"
                  [(ngModel)]="account.smtp_password"
                  name="smtp_password"
                  placeholder="Leave blank to use IMAP password"
                  autocomplete="new-password"
                />
                <button
                  type="button"
                  class="btn-toggle-password"
                  (click)="showSmtpPassword = !showSmtpPassword"
                  title="Toggle password visibility"
                >
                  <svg *ngIf="!showSmtpPassword" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                    <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/>
                    <circle cx="12" cy="12" r="3"/>
                  </svg>
                  <svg *ngIf="showSmtpPassword" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                    <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/>
                    <line x1="1" y1="1" x2="23" y2="23"/>
                  </svg>
                </button>
              </div>
            </div>
          </div>

          <div class="form-row">
            <div class="form-group flex-1">
              <label class="form-label" for="fromAddress">
                From Address / Header (Optional)
              </label>
              <input
                id="fromAddress"
                type="text"
                class="form-input"
                [(ngModel)]="account.from_address"
                name="from_address"
                placeholder='e.g. "Jane Doe" <jane@example.com>'
              />
              <div class="form-help">Custom display name and email for the "From:" header.</div>
            </div>
          </div>

          <div class="form-group">
            <label class="form-checkbox-label">
              <input
                type="checkbox"
                [(ngModel)]="account.smtp_use_tls"
                name="smtp_use_tls"
              />
              <span>Use TLS / STARTTLS encryption (recommended, port 587 or 465)</span>
            </label>
          </div>
        </div>

        <!-- Agent notifications -->
        <div class="card">
          <h3 class="card-section-title">Agent notifications</h3>
          <p class="section-help">
            Optional HTTPS webhook. New mail in watched folders is posted as an envelope summary so agents can wake without polling.
          </p>

          <div class="form-group">
            <label class="form-label" for="webhookUrl">Webhook URL</label>
            <input
              id="webhookUrl"
              type="url"
              class="form-input"
              [(ngModel)]="account.webhook_url"
              name="webhook_url"
              placeholder="https://example.com/webhooks/email"
              autocomplete="off"
            />
            <div class="form-help">Must be HTTPS to a public host. Clear the field to stop watching this account.</div>
          </div>

          <div class="form-group">
            <label class="form-label" for="webhookToken">
              Bearer token
              <span *ngIf="isEditMode && account.has_webhook_token" class="badge-green token-badge">Saved</span>
            </label>
            <div class="password-input-wrapper">
              <input
                id="webhookToken"
                [type]="showWebhookToken ? 'text' : 'password'"
                class="form-input"
                [(ngModel)]="account.webhook_token"
                name="webhook_token"
                [placeholder]="isEditMode ? 'Leave blank to keep current token' : 'Optional Authorization Bearer token'"
                autocomplete="new-password"
                (ngModelChange)="onWebhookTokenInput()"
              />
              <button
                type="button"
                class="btn-toggle-password"
                (click)="showWebhookToken = !showWebhookToken"
                title="Toggle token visibility"
              >
                <svg *ngIf="!showWebhookToken" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                  <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/>
                  <circle cx="12" cy="12" r="3"/>
                </svg>
                <svg *ngIf="showWebhookToken" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                  <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/>
                  <line x1="1" y1="1" x2="23" y2="23"/>
                </svg>
              </button>
            </div>
            <label *ngIf="isEditMode && account.has_webhook_token" class="form-checkbox-label token-clear">
              <input
                type="checkbox"
                [(ngModel)]="clearWebhookToken"
                name="clear_webhook_token"
                [disabled]="!!account.webhook_token"
              />
              <span>Remove saved token</span>
            </label>
            <div class="form-help">
              Leave blank to keep the current token. The saved token is dropped if you clear the webhook URL or change its origin (scheme + host). Use Remove saved token to drop authentication while keeping the URL.
            </div>
          </div>

          <div class="form-group">
            <label class="form-label" for="watchFolders">Watch folders</label>
            <input
              id="watchFolders"
              type="text"
              class="form-input"
              [(ngModel)]="watchFoldersText"
              name="watch_folders"
              placeholder="INBOX, Sent (default)"
            />
            <div class="form-help">
              Leave blank to watch INBOX and the account's Sent folder (resolved via IMAP special-use). Explicit names are used as-is — on Gmail, Sent is often [Gmail]/Sent Mail. Maximum 8 folders.
            </div>
          </div>
        </div>

        <!-- Connection Verification & Action Card -->
        <div class="card action-card">
          <div class="verify-section">
            <div class="verify-info">
              <h4 class="verify-title">Credential & Connection Verification</h4>
              <p class="verify-desc">
                Test live IMAP authentication and SMTP handshake using the last saved settings.
              </p>
              <span *ngIf="verifyHint(accountForm.dirty)" class="verify-hint">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
                  <circle cx="12" cy="12" r="10"/>
                  <line x1="12" y1="8" x2="12" y2="12"/>
                  <line x1="12" y1="16" x2="12.01" y2="16"/>
                </svg>
                {{ verifyHint(accountForm.dirty) }}
              </span>
            </div>

            <button
              type="button"
              id="verifyButton"
              class="btn btn-verify"
              [disabled]="!canVerify(accountForm.dirty) || verifying()"
              (click)="verifyConnection(accountForm.dirty)"
              title="Verify server connection and credentials"
            >
              <span *ngIf="verifying()" class="btn-spinner"></span>
              <svg *ngIf="!verifying()" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/>
                <polyline points="22 4 12 14.01 9 11.01"/>
              </svg>
              <span>{{ verifying() ? 'Verifying...' : 'Verify Connection' }}</span>
            </button>
          </div>

          <hr class="action-divider" />

          <div class="form-actions">
            <a routerLink="/" class="btn btn-secondary">
              Cancel
            </a>
            <button
              type="submit"
              class="btn btn-primary"
              [disabled]="saving() || !accountForm.form.valid || (!isEditMode && !account.imap_password)"
            >
              <span *ngIf="saving()" class="btn-spinner"></span>
              <span *ngIf="saving()">Saving...</span>
              <span *ngIf="!saving()">{{ isEditMode ? 'Update Account' : 'Save Account' }}</span>
            </button>
          </div>
        </div>
      </form>
    </div>
  `,
  styles: [`
    .page-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 24px;
      gap: 16px;
    }

    .page-subtitle {
      font-size: 14px;
      color: var(--gray-500);
      margin-top: 4px;
    }

    .card-section-title {
      font-size: 16px;
      color: var(--gray-900);
      margin-bottom: 4px;
    }

    .token-clear {
      margin-top: 8px;
    }

    .token-badge {
      margin-left: 8px;
      font-size: 11px;
      font-weight: 600;
      padding: 2px 6px;
      border-radius: var(--radius-sm);
    }

    .section-header-with-badge {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 4px;
    }

    .section-help {
      font-size: 13px;
      color: var(--gray-500);
      margin-bottom: 16px;
    }

    .preset-card {
      background-color: var(--gray-50);
      border-style: dashed;
    }

    .preset-buttons {
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      margin-bottom: 12px;
    }

    .preset-btn {
      font-size: 13px;
      padding: 6px 12px;
      background: white;
    }

    .preset-instructions {
      display: flex;
      align-items: flex-start;
      gap: 8px;
      font-size: 12px;
      color: #1e40af;
      background-color: var(--primary-light);
      padding: 8px 12px;
      border-radius: var(--radius-sm);
      border: 1px solid var(--primary-border);
    }

    .preset-instructions svg {
      flex-shrink: 0;
      margin-top: 2px;
    }

    .form-row {
      display: flex;
      gap: 16px;
      margin-bottom: 4px;
    }

    .flex-1 { flex: 1; }
    .flex-2 { flex: 2; }

    .password-input-wrapper {
      position: relative;
      display: flex;
      align-items: center;
    }

    .password-input-wrapper input {
      padding-right: 38px;
    }

    .btn-toggle-password {
      position: absolute;
      right: 8px;
      background: none;
      border: none;
      color: var(--gray-400);
      cursor: pointer;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 4px;
    }

    .btn-toggle-password:hover {
      color: var(--gray-600);
    }

    .action-card {
      background-color: var(--gray-50);
      border-color: var(--gray-300);
    }

    .verify-section {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 20px;
    }

    .verify-title {
      font-size: 15px;
      font-weight: 600;
      color: var(--gray-900);
      margin-bottom: 2px;
    }

    .verify-desc {
      font-size: 13px;
      color: var(--gray-600);
      margin-bottom: 4px;
    }

    .verify-hint {
      display: flex;
      align-items: center;
      gap: 6px;
      font-size: 12px;
      color: var(--warning-text);
      background-color: var(--warning-light);
      border: 1px solid var(--warning-border);
      padding: 4px 8px;
      border-radius: 4px;
      margin-top: 6px;
    }

    .btn-spinner {
      width: 14px;
      height: 14px;
      border: 2px solid rgba(255, 255, 255, 0.4);
      border-top-color: white;
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
    }

    @keyframes spin {
      to { transform: rotate(360deg); }
    }

    .action-divider {
      border: 0;
      border-top: 1px solid var(--gray-200);
      margin: 20px 0;
    }

    .form-actions {
      display: flex;
      justify-content: flex-end;
      gap: 12px;
    }

    .verification-content {
      width: 100%;
    }

    .verification-header {
      display: flex;
      align-items: center;
      gap: 8px;
      margin-bottom: 8px;
    }

    .verification-details {
      display: flex;
      flex-direction: column;
      gap: 4px;
      font-size: 13px;
    }

    .verify-item {
      display: flex;
      align-items: center;
      gap: 8px;
    }

    .verify-badge {
      font-weight: 600;
      font-size: 11px;
      padding: 1px 6px;
      border-radius: 4px;
    }

    .badge-danger {
      background-color: var(--danger-light);
      color: var(--danger-text);
      border: 1px solid var(--danger-border);
    }

    .loading-state {
      text-align: center;
      padding: 48px 16px;
      color: var(--gray-500);
    }

    .spinner {
      width: 36px;
      height: 36px;
      border: 3px solid var(--gray-200);
      border-top-color: var(--primary);
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
      margin: 0 auto 12px;
    }

    @media (max-width: 640px) {
      .form-row {
        flex-direction: column;
        gap: 0;
      }

      .verify-section {
        flex-direction: column;
        align-items: flex-start;
      }

      .page-header {
        flex-direction: column;
        align-items: flex-start;
      }
    }
  `]
})
export class AccountFormComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private router = inject(Router);
  private accountService = inject(AccountService);

  isEditMode = false;
  accountIdParam = '';
  loadingAccount = signal(false);
  saving = signal(false);
  verifying = signal(false);
  errorMessage = signal('');

  showImapPassword = false;
  showSmtpPassword = false;
  showWebhookToken = false;
  clearWebhookToken = false;
  watchFoldersText = '';

  presets: ProviderPreset[] = [];
  selectedPresetInstructions = '';

  verificationResult = signal<VerificationResult | null>(null);

  account: Account = {
    id: '',
    name: '',
    imap_host: '',
    imap_port: 993,
    imap_username: '',
    imap_password: '',
    imap_use_tls: true,
    smtp_host: '',
    smtp_port: 587,
    smtp_username: '',
    smtp_password: '',
    smtp_use_tls: true,
    from_address: '',
    webhook_url: '',
    webhook_token: '',
    watch_folders: []
  };

  ngOnInit(): void {
    this.presets = this.accountService.getPresets();
    this.accountIdParam = this.route.snapshot.paramMap.get('id') || '';

    if (this.accountIdParam) {
      this.isEditMode = true;
      this.loadAccount(this.accountIdParam);
    }
  }

  loadAccount(id: string): void {
    this.loadingAccount.set(true);
    this.errorMessage.set('');

    this.accountService.getAccount(id).pipe(
      finalize(() => this.loadingAccount.set(false))
    ).subscribe({
      next: (acc) => {
        this.account = {
          ...acc,
          imap_password: '', // Blank initially in edit mode
          smtp_password: '',
          webhook_token: ''
        };
        this.clearWebhookToken = false;
        this.watchFoldersText = foldersToText(acc.watch_folders);
      },
      error: (err) => {
        this.errorMessage.set(err?.error?.error || `Failed to load account ${id}.`);
      }
    });
  }

  applyPreset(preset: ProviderPreset): void {
    this.account.imap_host = preset.imap_host;
    this.account.imap_port = preset.imap_port;
    this.account.imap_use_tls = preset.imap_use_tls;
    this.account.smtp_host = preset.smtp_host;
    this.account.smtp_port = preset.smtp_port;
    this.account.smtp_use_tls = preset.smtp_use_tls;
    this.selectedPresetInstructions = preset.instructions || '';
  }

  onNameChange(): void {
    if (!this.isEditMode && !this.account.id) {
      // Auto-generate a clean ID from name
      this.account.id = this.account.name
        .toLowerCase()
        .replace(/[^a-z0-9_-]/g, '-')
        .replace(/-+/g, '-')
        .replace(/^-|-$/g, '');
    }
  }

  onUsernameChange(): void {
    if (!this.account.smtp_username && this.account.imap_username) {
      // Convenience: auto-fill smtp_username if empty
      // this.account.smtp_username = this.account.imap_username;
    }
  }

  /**
   * Verify is available only for a saved account whose form has no unsaved changes.
   * Verification always uses the previously stored settings, not the live form.
   */
  canVerify(formDirty: boolean | null = false): boolean {
    return this.isEditMode && !!this.account.id && !!this.account.has_password && !formDirty;
  }

  verifyHint(formDirty: boolean | null = false): string {
    if (!this.isEditMode || !this.account.id) {
      return 'Save the account first, then verify the stored connection.';
    }
    if (formDirty) {
      return 'Save your changes before verifying. Verification uses the last saved settings.';
    }
    if (!this.account.has_password) {
      return 'This account has no stored password to verify.';
    }
    return '';
  }

  verifyConnection(formDirty: boolean | null = false): void {
    if (!this.canVerify(formDirty) || this.verifying()) return;

    this.verifying.set(true);
    this.verificationResult.set(null);
    this.errorMessage.set('');

    this.accountService.verifyAccount(this.account.id).pipe(
      finalize(() => this.verifying.set(false))
    ).subscribe({
      next: (res) => {
        this.verificationResult.set(res);
      },
      error: (err) => {
        const message = httpErrorMessage(err, 'Verification request failed');
        this.verificationResult.set({
          success: false,
          imap: { success: false, error: message },
          smtp: { success: false, error: message }
        });
      }
    });
  }

  onWebhookTokenInput(): void {
    if (this.account.webhook_token) {
      this.clearWebhookToken = false;
    }
  }

  saveAccount(): void {
    if (this.saving()) return;

    this.errorMessage.set('');
    const watchFolders = parseWatchFolders(this.watchFoldersText);
    if (watchFolders.length > MAX_WATCH_FOLDERS) {
      this.errorMessage.set(`At most ${MAX_WATCH_FOLDERS} watch folders are allowed.`);
      return;
    }

    this.saving.set(true);

    // Clean up input types
    const payload: Partial<Account> = {
      id: this.account.id.trim(),
      name: this.account.name.trim(),
      imap_host: this.account.imap_host.trim(),
      imap_port: Number(this.account.imap_port),
      imap_username: this.account.imap_username.trim(),
      imap_use_tls: this.account.imap_use_tls,
      smtp_host: this.account.smtp_host.trim(),
      smtp_port: Number(this.account.smtp_port),
      smtp_username: this.account.smtp_username ? this.account.smtp_username.trim() : '',
      smtp_use_tls: this.account.smtp_use_tls,
      from_address: this.account.from_address ? this.account.from_address.trim() : '',
      webhook_url: this.account.webhook_url ? this.account.webhook_url.trim() : '',
      watch_folders: watchFolders
    };

    if (this.account.imap_password && this.account.imap_password.trim()) {
      payload.imap_password = this.account.imap_password.trim();
    }
    if (this.account.smtp_password && this.account.smtp_password.trim()) {
      payload.smtp_password = this.account.smtp_password.trim();
    }
    if (this.account.webhook_token && this.account.webhook_token.trim()) {
      payload.webhook_token = this.account.webhook_token.trim();
    } else if (this.clearWebhookToken) {
      payload.clear_webhook_token = true;
    }

    const saveObs = this.isEditMode
      ? this.accountService.updateAccount(this.account.id, payload)
      : this.accountService.createAccount(payload);

    saveObs.pipe(
      finalize(() => this.saving.set(false))
    ).subscribe({
      next: () => {
        this.router.navigate(['/']);
      },
      error: (err) => {
        this.errorMessage.set(httpErrorMessage(err, 'Failed to save email account.'));
      }
    });
  }
}

const MAX_WATCH_FOLDERS = 8;

function parseWatchFolders(text: string): string[] {
  return text
    .split(',')
    .map((folder) => folder.trim())
    .filter((folder) => folder.length > 0);
}

function foldersToText(folders?: string[]): string {
  if (!folders || folders.length === 0) {
    return '';
  }
  return folders.join(', ');
}

function httpErrorMessage(err: unknown, fallback: string): string {
  const e = err as { name?: string; message?: string; error?: { error?: string } };
  if (e?.name === 'TimeoutError') {
    return 'The request timed out. Check host, port, and network connectivity.';
  }
  return e?.error?.error || fallback;
}
