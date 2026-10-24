import { Component, inject, OnInit, signal } from '@angular/core';
import { CommonModule } from '@angular/common';
import { Router, RouterLink } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { finalize } from 'rxjs';
import { AccountService } from '../../services/account.service';
import { AuthService } from '../../services/auth.service';
import { AccountSummary } from '../../models/account.model';
import { ConfirmDialogComponent } from '../confirm-dialog/confirm-dialog.component';

@Component({
  selector: 'app-account-list',
  standalone: true,
  imports: [CommonModule, RouterLink, FormsModule, ConfirmDialogComponent],
  template: `
    <div class="container">
      <div class="page-header">
        <div>
          <h2>Email Accounts</h2>
          <p class="page-subtitle">
            Manage your configured IMAP and SMTP email accounts.
          </p>
        </div>
        <a routerLink="/accounts/new" class="btn btn-primary">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
            <line x1="12" y1="5" x2="12" y2="19"/>
            <line x1="5" y1="12" x2="19" y2="12"/>
          </svg>
          Add Email Account
        </a>
      </div>

      <!-- Alerts -->
      <div *ngIf="successMessage()" class="alert alert-success">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18">
          <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/>
          <polyline points="22 4 12 14.01 9 11.01"/>
        </svg>
        <span>{{ successMessage() }}</span>
      </div>

      <div *ngIf="errorMessage()" class="alert alert-danger">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18">
          <circle cx="12" cy="12" r="10"/>
          <line x1="12" y1="8" x2="12" y2="12"/>
          <line x1="12" y1="16" x2="12.01" y2="16"/>
        </svg>
        <span>{{ errorMessage() }}</span>
      </div>

      <!-- Loading State -->
      <div *ngIf="loading()" class="loading-state">
        <div class="spinner"></div>
        <p>Loading email accounts...</p>
      </div>

      <!-- Account List Content -->
      <div *ngIf="!loading()">
        <!-- Empty State -->
        <div *ngIf="accounts().length === 0" class="empty-state card">
          <div class="empty-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
              <rect width="20" height="16" x="2" y="4" rx="2"/>
              <path d="m22 7-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 7"/>
            </svg>
          </div>
          <h3>No Email Accounts Configured</h3>
          <p>
            You haven't configured any email accounts yet. Add an email account so EmailMCP can securely access your mailbox.
          </p>
          <a routerLink="/accounts/new" class="btn btn-primary">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
              <line x1="12" y1="5" x2="12" y2="19"/>
              <line x1="5" y1="12" x2="19" y2="12"/>
            </svg>
            Add Your First Account
          </a>
        </div>

        <!-- Accounts Grid -->
        <div *ngIf="accounts().length > 0" class="accounts-grid">
          <div *ngFor="let acc of accounts()" class="account-card card">
            <div class="account-card-header">
              <div>
                <div class="account-title-row">
                  <h3 class="account-name">{{ acc.name }}</h3>
                  <span class="badge badge-gray">{{ acc.id }}</span>
                </div>
                <div class="account-username">
                  {{ acc.imap_username }}
                </div>
              </div>
              <div class="account-actions">
                <button (click)="editAccount(acc.id)" class="btn btn-secondary btn-sm" title="Edit account">
                  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
                    <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/>
                    <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/>
                  </svg>
                  Edit
                </button>
                <button (click)="openDeleteDialog(acc)" class="btn btn-outline-danger btn-sm" title="Delete account">
                  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
                    <polyline points="3 6 5 6 21 6"/>
                    <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
                  </svg>
                  Delete
                </button>
              </div>
            </div>

            <div class="account-details">
              <div class="detail-row">
                <span class="detail-label">IMAP Server:</span>
                <span class="detail-value">
                  <code>{{ acc.imap_host }}:{{ acc.imap_port }}</code>
                  <span *ngIf="acc.imap_use_tls" class="badge badge-green">TLS</span>
                  <span *ngIf="!acc.imap_use_tls" class="badge badge-gray">Plain</span>
                </span>
              </div>

              <div class="detail-row">
                <span class="detail-label">SMTP Server:</span>
                <span class="detail-value">
                  <code>{{ acc.smtp_host }}:{{ acc.smtp_port }}</code>
                  <span *ngIf="acc.smtp_use_tls" class="badge badge-green">TLS</span>
                  <span *ngIf="!acc.smtp_use_tls" class="badge badge-gray">Plain</span>
                </span>
              </div>

              <div *ngIf="acc.from_address" class="detail-row">
                <span class="detail-label">From Header:</span>
                <span class="detail-value">{{ acc.from_address }}</span>
              </div>

              <div class="detail-row status-row">
                <span class="detail-label">Credentials:</span>
                <span class="detail-value">
                  <span *ngIf="acc.has_password" class="badge badge-blue">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="12" height="12">
                      <rect width="18" height="11" x="3" y="11" rx="2" ry="2"/>
                      <path d="M7 11V7a5 5 0 0 1 10 0v4"/>
                    </svg>
                    Password Saved
                  </span>
                  <span *ngIf="!acc.has_password" class="badge badge-gray">No Password</span>
                </span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- Delete Confirmation Dialog -->
      <app-confirm-dialog
        [isOpen]="isDeleteDialogOpen()"
        title="Delete Email Account"
        [message]="getDeleteMessage()"
        confirmText="Delete Account"
        [loading]="deleting()"
        (confirm)="confirmDelete()"
        (cancel)="closeDeleteDialog()"
      ></app-confirm-dialog>
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

    @keyframes spin {
      to { transform: rotate(360deg); }
    }

    .empty-state {
      text-align: center;
      padding: 48px 24px;
    }

    .empty-icon {
      width: 64px;
      height: 64px;
      border-radius: 20px;
      background: var(--gray-100);
      color: var(--gray-400);
      display: flex;
      align-items: center;
      justify-content: center;
      margin: 0 auto 16px;
    }

    .empty-icon svg {
      width: 32px;
      height: 32px;
    }

    .empty-state h3 {
      font-size: 18px;
      margin-bottom: 8px;
    }

    .empty-state p {
      font-size: 14px;
      color: var(--gray-500);
      max-width: 440px;
      margin: 0 auto 20px;
    }

    .accounts-grid {
      display: flex;
      flex-direction: column;
      gap: 16px;
    }

    .account-card {
      padding: 20px;
      margin-bottom: 0;
      transition: box-shadow 0.15s ease, border-color 0.15s ease;
    }

    .account-card:hover {
      border-color: var(--gray-300);
      box-shadow: var(--shadow-md);
    }

    .account-card-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 16px;
      gap: 16px;
    }

    .account-title-row {
      display: flex;
      align-items: center;
      gap: 8px;
      margin-bottom: 4px;
    }

    .account-name {
      font-size: 16px;
      color: var(--gray-900);
    }

    .account-username {
      font-size: 13px;
      color: var(--gray-600);
    }

    .account-actions {
      display: flex;
      gap: 8px;
    }

    .btn-sm {
      padding: 5px 10px;
      font-size: 13px;
    }

    .account-details {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 10px 28px;
      background-color: var(--gray-50);
      padding: 12px 16px;
      border-radius: var(--radius-sm);
      font-size: 13px;
    }

    .detail-row {
      display: flex;
      align-items: center;
      gap: 8px;
      min-width: 0;
      white-space: nowrap;
    }

    .detail-label {
      color: var(--gray-500);
      font-weight: 500;
      flex-shrink: 0;
    }

    .detail-value {
      color: var(--gray-800);
      display: flex;
      align-items: center;
      gap: 6px;
      min-width: 0;
    }

    .detail-value code {
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 12px;
      background-color: white;
      padding: 2px 6px;
      border-radius: 4px;
      border: 1px solid var(--gray-200);
      white-space: nowrap;
    }

    .status-row {
      flex: 1 0 100%;
    }

    @media (max-width: 640px) {
      .page-header {
        flex-direction: column;
        align-items: flex-start;
      }

      .account-card-header {
        flex-direction: column;
      }

      .account-actions {
        width: 100%;
        justify-content: flex-end;
      }

      .account-details {
        flex-direction: column;
        align-items: stretch;
      }

      .detail-row {
        white-space: nowrap;
        overflow: hidden;
      }

      .detail-value code {
        overflow: hidden;
        text-overflow: ellipsis;
        max-width: 100%;
      }
    }
  `]
})
export class AccountListComponent implements OnInit {
  private accountService = inject(AccountService);
  private authService = inject(AuthService);
  private router = inject(Router);

  accounts = signal<AccountSummary[]>([]);
  loading = signal(true);
  errorMessage = signal('');
  successMessage = signal('');

  isDeleteDialogOpen = signal(false);
  selectedAccount: AccountSummary | null = null;
  deleting = signal(false);

  ngOnInit(): void {
    this.loadAccounts();
  }

  loadAccounts(): void {
    this.loading.set(true);
    this.errorMessage.set('');

    this.accountService.getAccounts().pipe(
      finalize(() => this.loading.set(false))
    ).subscribe({
      next: (accounts) => {
        this.accounts.set(Array.isArray(accounts) ? accounts : []);
      },
      error: (err) => {
        this.accounts.set([]);
        if (this.authService.isSessionExpiredError(err)) return;
        this.errorMessage.set(err?.error?.error || 'Failed to load email accounts.');
      }
    });
  }

  editAccount(id: string): void {
    this.router.navigate(['/accounts/edit', id]);
  }

  openDeleteDialog(acc: AccountSummary): void {
    this.selectedAccount = acc;
    this.isDeleteDialogOpen.set(true);
  }

  closeDeleteDialog(): void {
    if (this.deleting()) return;
    this.isDeleteDialogOpen.set(false);
    this.selectedAccount = null;
  }

  getDeleteMessage(): string {
    const name = this.selectedAccount?.name || 'this account';
    return `Are you sure you want to delete the email account "${name}"? This will remove the stored credentials.`;
  }

  confirmDelete(): void {
    if (!this.selectedAccount || this.deleting()) return;

    this.deleting.set(true);
    this.errorMessage.set('');
    this.successMessage.set('');

    const name = this.selectedAccount.name;
    this.accountService.deleteAccount(this.selectedAccount.id).pipe(
      finalize(() => this.deleting.set(false))
    ).subscribe({
      next: () => {
        this.isDeleteDialogOpen.set(false);
        this.selectedAccount = null;
        this.successMessage.set(`Account "${name}" deleted successfully.`);
        this.loadAccounts();
      },
      error: (err) => {
        this.isDeleteDialogOpen.set(false);
        this.selectedAccount = null;
        if (this.authService.isSessionExpiredError(err)) return;
        this.errorMessage.set(err?.error?.error || `Failed to delete account "${name}".`);
      }
    });
  }
}
