import { Module, Global } from '@nestjs/common';
import { ConfigService } from '@nestjs/config';
import { CodeQClient } from '@codeq/sdk';

/**
 * Global module providing CodeQ client.
 * 
 * Configures and exports CodeQClient for dependency injection
 * across the application.
 */
@Global()
@Module({
  providers: [
    {
      provide: 'CODEQ_CLIENT',
      useFactory: (configService: ConfigService) => {
        return new CodeQClient({
          baseUrl: configService.get<string>('CODEQ_BASE_URL'),
          producerToken: configService.get<string>('CODEQ_PRODUCER_TOKEN'),
          workerToken: configService.get<string>('CODEQ_WORKER_TOKEN'),
          timeout: 30000,
          retries: 3,
        });
      },
      inject: [ConfigService],
    },
  ],
  exports: ['CODEQ_CLIENT'],
})
export class CodeQModule {}
