import { Injectable, signal, computed } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { Observable, catchError, map, of, tap } from 'rxjs';
import { UserInfo } from '../models/account.model';

const TOKEN_KEY = 'emailmcp_access_token';

@Injectable({
  providedIn: 'root'
})
export class AuthService {
  private userSignal = signal<UserInfo | null>(null);
  private loadingSignal = signal<boolean>(true);

  readonly user = computed(() => this.userSignal());
  readonly isAuthenticated = computed(() => !!this.userSignal()?.authenticated);
  readonly isLoading = computed(() => this.loadingSignal());

  constructor(private http: HttpClient) {
    this.initAuth();
  }

  private initAuth(): void {
    // Check if token was provided in URL query param (e.g. after OAuth callback redirect)
    const urlParams = new URLSearchParams(window.location.search);
    const urlToken = urlParams.get('token');
    if (urlToken) {
      this.setToken(urlToken);
      // Clean up URL without reloading
      urlParams.delete('token');
      const newQuery = urlParams.toString();
      const newUrl = window.location.pathname + (newQuery ? `?${newQuery}` : '') + window.location.hash;
      window.history.replaceState({}, document.title, newUrl);
    }

    this.checkAuth().subscribe();
  }

  getToken(): string | null {
    return localStorage.getItem(TOKEN_KEY) || sessionStorage.getItem(TOKEN_KEY);
  }

  isSessionExpiredError(error: unknown): boolean {
    const body = error && typeof error === 'object' && 'error' in error
      ? (error as { error?: unknown }).error
      : error;

    if (typeof body === 'string') {
      return body.trim() === 'Session expired';
    }

    return !!body && typeof body === 'object' && 'error' in body
      && (body as { error?: unknown }).error === 'Session expired';
  }

  setToken(token: string, remember: boolean = true): void {
    if (remember) {
      localStorage.setItem(TOKEN_KEY, token);
    } else {
      sessionStorage.setItem(TOKEN_KEY, token);
    }
  }

  clearToken(): void {
    localStorage.removeItem(TOKEN_KEY);
    sessionStorage.removeItem(TOKEN_KEY);
    this.userSignal.set(null);
  }

  checkAuth(): Observable<UserInfo | null> {
    this.loadingSignal.set(true);
    return this.http.get<UserInfo>('/api/me').pipe(
      tap((userInfo) => {
        this.userSignal.set(userInfo);
        this.loadingSignal.set(false);
      }),
      catchError(() => {
        this.userSignal.set(null);
        this.loadingSignal.set(false);
        return of(null);
      })
    );
  }

  loginWithToken(token: string): Observable<boolean> {
    this.setToken(token);
    return this.checkAuth().pipe(
      map((user) => !!user?.authenticated)
    );
  }

  loginWithGoogle(): void {
    window.location.href = '/auth/login';
  }

  logout(): Observable<void> {
    return this.http.post<void>('/api/logout', {}).pipe(
      catchError(() => of(undefined as void)),
      tap(() => {
        this.clearToken();
      })
    );
  }
}
