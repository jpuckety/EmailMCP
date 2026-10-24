import { HttpErrorResponse, HttpInterceptorFn } from '@angular/common/http';
import { inject } from '@angular/core';
import { Router } from '@angular/router';
import { catchError, throwError } from 'rxjs';
import { AuthService } from './auth.service';

export const authInterceptor: HttpInterceptorFn = (req, next) => {
  const authService = inject(AuthService);
  const router = inject(Router);
  const token = authService.getToken();

  let modifiedReq = req.clone({
    withCredentials: true
  });

  if (token && req.url.startsWith('/api/')) {
    modifiedReq = modifiedReq.clone({
      setHeaders: {
        Authorization: `Bearer ${token}`
      }
    });
  }

  return next(modifiedReq).pipe(catchError((error: unknown) => {
    if (req.url.startsWith('/api/')
      && error instanceof HttpErrorResponse
      && error.status === 401
      && authService.isSessionExpiredError(error)) {
      authService.clearToken();
      if (router.url.split(/[?#]/, 1)[0] !== '/login') {
        void router.navigate(['/login']);
      }
    }

    return throwError(() => error);
  }));
};
